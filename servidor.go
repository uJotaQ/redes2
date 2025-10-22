package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"pbl_redes/protocolo"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb"
)

// --- Constantes de Cores para Logs ---
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorCyan   = "\033[36m"
)

// --- Logger Personalizado ---
var (
	logger *log.Logger
)

// --- ESTRUTURAS DE DADOS DO SERVIDOR ---

type User struct {
	Login              string
	Senha              string
	Conn               net.Conn
	Online             bool
	Inventario         protocolo.Inventario
	Moedas             int
	Latencia           int64
	SelectedInstrument *protocolo.Instrumento
}

type GameState struct {
	Player1Score int
	Player2Score int
	PlayedNotes  []string
	CurrentTurn  int
	GameMutex    sync.Mutex
	LastAttacker *User
}

type Sala struct {
	ID        string
	Jogador1  net.Conn
	Jogador2  net.Conn
	Status    string
	IsPrivate bool
	Game      *GameState
}

// --- VARIÁVEIS GLOBAIS ---

var (
	salas              map[string]*Sala
	salasEmEspera      []*Sala
	playersInRoom      map[string]*Sala
	players            map[string]*User
	instrumentDatabase []protocolo.Instrumento
	packetStock        map[string]*protocolo.Packet
	stockMu            sync.RWMutex
	mu                 sync.Mutex
	mqttClient         mqtt.Client
	raftNode           *raft.Raft
)

// --- RAFT FSM (FINITE STATE MACHINE) ---
type FSM struct{}

func (f *FSM) Apply(log *raft.Log) interface{} {
	// logger.Printf("FSM.Apply: aplicando log: %s", string(log.Data))
	return nil
}
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) { return &fsmSnapshot{}, nil }
func (f *FSM) Restore(rc io.ReadCloser) error      { return nil }

type fsmSnapshot struct{}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error { return sink.Close() }
func (s *fsmSnapshot) Release()                             {}

// --- LÓGICA DE PUBLICAÇÃO MQTT ---

func publishToPlayer(salaID, login string, message protocolo.Message) {
	if mqttClient == nil || !mqttClient.IsConnected() {
		logger.Println(ColorYellow + "AVISO: Cliente MQTT não está conectado, pulando publicação." + ColorReset)
		return
	}
	topic := fmt.Sprintf("game/%s/%s", salaID, login)
	payload, _ := json.Marshal(message)
	token := mqttClient.Publish(topic, 0, false, payload)
	go func() {
		_ = token.Wait()
		if token.Error() != nil {
			logger.Printf(ColorRed+"Erro ao publicar no tópico %s: %v"+ColorReset, topic, token.Error())
		}
	}()
}

// --- PERSISTÊNCIA DE DADOS ---
const playerDataFile = "data/players.json"
const instrumentDataFile = "data/instrumentos.json"

func loadPlayerData() {
	mu.Lock()
	defer mu.Unlock()
	data, err := os.ReadFile(playerDataFile)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Printf("Arquivo de jogadores (%s) não encontrado. Um novo será criado.", playerDataFile)
			players = make(map[string]*User)
		}
		return
	}
	json.Unmarshal(data, &players)
	logger.Printf("%d jogadores carregados.", len(players))
}

func savePlayerData() {
	logger.Println("\nSalvando dados dos jogadores...")
	mu.Lock()
	defer mu.Unlock()
	for _, player := range players {
		player.Online = false
		player.Conn = nil
	}
	data, _ := json.MarshalIndent(players, "", "  ")
	os.WriteFile(playerDataFile, data, 0644)
	logger.Printf("Dados de %d jogadores salvos.", len(players))
}

// --- LÓGICA DE LOGIN E CADASTRO ---

func loginUser(conn net.Conn, data protocolo.LoginRequest) {
	mu.Lock()
	defer mu.Unlock()
	player, exists := players[data.Login]
	if !exists {
		sendJSON(conn, protocolo.Message{Type: "LOGIN", Data: protocolo.LoginResponse{Status: "N_EXIST"}})
		return
	}
	if player.Online {
		sendJSON(conn, protocolo.Message{Type: "LOGIN", Data: protocolo.LoginResponse{Status: "ONLINE_JA"}})
		return
	}
	player.Conn = conn
	player.Online = true
	msg := protocolo.Message{
		Type: "LOGIN",
		Data: protocolo.LoginResponse{
			Status:     "LOGADO",
			Inventario: player.Inventario,
			Saldo:      player.Moedas,
		},
	}
	sendJSON(conn, msg)
}

func cadastrarUser(conn net.Conn, data protocolo.SignInRequest) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := players[data.Login]; exists {
		sendScreenMsg(conn, "Login já existe.")
		return
	}
	players[data.Login] = &User{
		Login:  data.Login,
		Senha:  data.Senha,
		Moedas: 100, // Saldo inicial
	}
	sendScreenMsg(conn, "Cadastro realizado com sucesso!")
}

// --- FUNÇÕES AUXILIARES ---

func sendJSON(conn net.Conn, msg protocolo.Message) {
	if conn == nil {
		return
	}
	jsonData, _ := json.Marshal(msg)
	conn.Write(jsonData)
	conn.Write([]byte("\n"))
}

func mapToStruct(input interface{}, target interface{}) error {
	bytes, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func randomGenerate(length int) string {
	const charset = "ACDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

func findPlayerByConn(conn net.Conn) *User {
	for _, player := range players {
		if player.Conn == conn {
			return player
		}
	}
	return nil
}

func sendScreenMsg(conn net.Conn, text string) {
	msg := protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: text}}
	sendJSON(conn, msg)
}

// --- LÓGICA DE SALAS E PAREAMENTO ---

func findRoom(conn net.Conn, mode string, roomCode string) {
	mu.Lock()
	defer mu.Unlock()
	player := findPlayerByConn(conn)
	if player.SelectedInstrument == nil {
		sendScreenMsg(conn, "Você precisa selecionar um instrumento para a batalha primeiro!")
		return
	}
	if mode == "PUBLIC" {
		if len(salasEmEspera) > 0 {
			sala := salasEmEspera[0]
			salasEmEspera = salasEmEspera[1:]
			sala.Jogador2 = conn
			sala.Status = "Em_Jogo"
			playersInRoom[sala.Jogador1.RemoteAddr().String()] = sala
			playersInRoom[sala.Jogador2.RemoteAddr().String()] = sala
			sendPairing(sala.Jogador1)
			sendPairing(sala.Jogador2)
			go startGame(sala)
		} else {
			codigo := randomGenerate(6)
			novaSala := &Sala{Jogador1: conn, ID: codigo, Status: "Waiting_Player", IsPrivate: false}
			salas[codigo] = novaSala
			salasEmEspera = append(salasEmEspera, novaSala)
			playersInRoom[conn.RemoteAddr().String()] = novaSala
		}
	} else if roomCode != "" {
		sala, ok := salas[roomCode]
		if !ok || !sala.IsPrivate || sala.Jogador2 != nil {
			sendScreenMsg(conn, "Código inválido ou sala cheia.")
			return
		}
		sala.Jogador2 = conn
		sala.Status = "Em_Jogo"
		playersInRoom[sala.Jogador1.RemoteAddr().String()] = sala
		playersInRoom[sala.Jogador2.RemoteAddr().String()] = sala
		sendPairing(sala.Jogador1)
		sendPairing(sala.Jogador2)
		go startGame(sala)
	}
}

func createRoom(conn net.Conn) {
	mu.Lock()
	defer mu.Unlock()
	player := findPlayerByConn(conn)
	if player.SelectedInstrument == nil {
		sendScreenMsg(conn, "Você precisa selecionar um instrumento para a batalha primeiro!")
		return
	}
	codigo := randomGenerate(6)
	novaSala := &Sala{Jogador1: conn, ID: codigo, Status: "Waiting_Player", IsPrivate: true}
	salas[codigo] = novaSala
	playersInRoom[conn.RemoteAddr().String()] = novaSala
	sendScreenMsg(conn, "Sala privada criada. Código: "+codigo)
}

func sendPairing(conn net.Conn) {
	p := findPlayerByConn(conn)
	sala := playersInRoom[conn.RemoteAddr().String()]
	msg := protocolo.Message{Type: "PAREADO", Data: protocolo.PairingMessage{Status: "PAREADO", SalaID: sala.ID, PlayerLogin: p.Login}}
	sendJSON(conn, msg)
}

// --- LÓGICA DO JOGO MUSICAL ---

func startGame(sala *Sala) {
	p1 := findPlayerByConn(sala.Jogador1)
	p2 := findPlayerByConn(sala.Jogador2)
	if p1 == nil || p2 == nil {
		return
	}

	sala.Game = &GameState{
		CurrentTurn: 1,
	}

	publishToPlayer(sala.ID, p1.Login, protocolo.Message{Type: "GAME_START", Data: protocolo.GameStartMessage{Opponent: p2.Login}})
	publishToPlayer(sala.ID, p2.Login, protocolo.Message{Type: "GAME_START", Data: protocolo.GameStartMessage{Opponent: p1.Login}})

	time.Sleep(1 * time.Second)
	notifyTurn(sala)
}

func notifyTurn(sala *Sala) {
	p1 := findPlayerByConn(sala.Jogador1)
	p2 := findPlayerByConn(sala.Jogador2)
	if p1 == nil || p2 == nil {
		return
	}

	publishToPlayer(sala.ID, p1.Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: sala.Game.CurrentTurn == 1}})
	publishToPlayer(sala.ID, p2.Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: sala.Game.CurrentTurn == 2}})
}

func handlePlayNote(conn net.Conn, data interface{}) {
	var req protocolo.PlayNoteRequest
	mapToStruct(data, &req)

	mu.Lock()
	sala, ok := playersInRoom[conn.RemoteAddr().String()]
	mu.Unlock()

	if !ok || sala.Game == nil {
		sendScreenMsg(conn, "Você não está em um jogo ativo.")
		return
	}

	sala.Game.GameMutex.Lock()
	defer sala.Game.GameMutex.Unlock()

	p := findPlayerByConn(conn)
	isPlayer1 := conn == sala.Jogador1
	playerNum := 2
	if isPlayer1 {
		playerNum = 1
	}

	if sala.Game.CurrentTurn != playerNum {
		sendScreenMsg(conn, "Não é a sua vez!")
		return
	}

	validNotes := "ABCDEFG"
	note := strings.ToUpper(req.Note)
	if len(note) != 1 || !strings.Contains(validNotes, note) {
		sendScreenMsg(conn, "Nota inválida! Use A, B, C, D, E, F ou G.")
		return
	}

	sala.Game.PlayedNotes = append(sala.Game.PlayedNotes, note)

	p1 := findPlayerByConn(sala.Jogador1)
	p2 := findPlayerByConn(sala.Jogador2)

	attackName, attacker := checkAttackCompletion(sala)
	sequenceStr := strings.Join(sala.Game.PlayedNotes, "-")

	resultMsgBase := protocolo.RoundResultMessage{
		PlayedNote:      note,
		PlayerName:      p.Login,
		CurrentSequence: sequenceStr,
		AttackTriggered: attacker != nil,
	}

	if attacker != nil {
		resultMsgBase.AttackName = attackName
		resultMsgBase.AttackerName = attacker.Login
		if attacker == p1 {
			sala.Game.Player1Score++
		} else {
			sala.Game.Player2Score++
		}
		sala.Game.PlayedNotes = []string{}
		sala.Game.LastAttacker = attacker
	}

	resultMsgP1 := resultMsgBase
	resultMsgP1.YourScore = sala.Game.Player1Score
	resultMsgP1.OpponentScore = sala.Game.Player2Score
	publishToPlayer(sala.ID, p1.Login, protocolo.Message{Type: "ROUND_RESULT", Data: resultMsgP1})

	resultMsgP2 := resultMsgBase
	resultMsgP2.YourScore = sala.Game.Player2Score
	resultMsgP2.OpponentScore = sala.Game.Player1Score
	publishToPlayer(sala.ID, p2.Login, protocolo.Message{Type: "ROUND_RESULT", Data: resultMsgP2})

	if sala.Game.Player1Score >= 2 || sala.Game.Player2Score >= 2 {
		endGame(sala)
		return
	}

	if attacker != nil {
		if attacker == p1 {
			sala.Game.CurrentTurn = 1
		} else {
			sala.Game.CurrentTurn = 2
		}
	} else {
		if sala.Game.CurrentTurn == 1 {
			sala.Game.CurrentTurn = 2
		} else {
			sala.Game.CurrentTurn = 1
		}
	}

	time.Sleep(500 * time.Millisecond)
	notifyTurn(sala)
}

func checkAttackCompletion(sala *Sala) (string, *User) {
	p1 := findPlayerByConn(sala.Jogador1)
	p2 := findPlayerByConn(sala.Jogador2)
	sequence := strings.Join(sala.Game.PlayedNotes, "")

	if p1.SelectedInstrument != nil {
		for _, attack := range p1.SelectedInstrument.Attacks {
			attackSeq := strings.Join(attack.Sequence, "")
			if strings.HasSuffix(sequence, attackSeq) {
				return attack.Name, p1
			}
		}
	}
	if p2.SelectedInstrument != nil {
		for _, attack := range p2.SelectedInstrument.Attacks {
			attackSeq := strings.Join(attack.Sequence, "")
			if strings.HasSuffix(sequence, attackSeq) {
				return attack.Name, p2
			}
		}
	}
	return "", nil
}

func endGame(sala *Sala) {
	game := sala.Game
	p1 := findPlayerByConn(sala.Jogador1)
	p2 := findPlayerByConn(sala.Jogador2)

	p1.Moedas += game.Player1Score * 5
	p2.Moedas += game.Player2Score * 5

	var winner string
	if game.Player1Score > game.Player2Score {
		winner = p1.Login
		p1.Moedas += 20
	} else if game.Player2Score > game.Player1Score {
		winner = p2.Login
		p2.Moedas += 20
	} else {
		winner = "EMPATE"
	}

	gameOverMsgP1 := protocolo.GameOverMessage{
		Winner:       winner,
		FinalScoreP1: game.Player1Score,
		FinalScoreP2: game.Player2Score,
		CoinsEarned: game.Player1Score*5 + (func() int {
			if winner == p1.Login {
				return 20
			}
			return 0
		}()),
	}
	publishToPlayer(sala.ID, p1.Login, protocolo.Message{Type: "GAME_OVER", Data: gameOverMsgP1})

	gameOverMsgP2 := protocolo.GameOverMessage{
		Winner:       winner,
		FinalScoreP1: game.Player1Score,
		FinalScoreP2: game.Player2Score,
		CoinsEarned: game.Player2Score*5 + (func() int {
			if winner == p2.Login {
				return 20
			}
			return 0
		}()),
	}
	publishToPlayer(sala.ID, p2.Login, protocolo.Message{Type: "GAME_OVER", Data: gameOverMsgP2})

	mu.Lock()
	delete(playersInRoom, sala.Jogador1.RemoteAddr().String())
	if sala.Jogador2 != nil {
		delete(playersInRoom, sala.Jogador2.RemoteAddr().String())
	}
	delete(salas, sala.ID)
	mu.Unlock()
}

// --- LÓGICA DE PACOTES E INSTRUMENTOS ---

func loadInstruments() error {
	data, err := os.ReadFile(instrumentDataFile)
	if err != nil {
		return fmt.Errorf("erro ao ler o arquivo de instrumentos: %v", err)
	}
	if err := json.Unmarshal(data, &instrumentDatabase); err != nil {
		return fmt.Errorf("erro ao decodificar o JSON dos instrumentos: %v", err)
	}
	logger.Printf("Carregados %d instrumentos da base de dados.", len(instrumentDatabase))
	return nil
}

func GetRandomInstrumentByRarity(rarity string) *protocolo.Instrumento {
	var filtered []protocolo.Instrumento
	for _, inst := range instrumentDatabase {
		if inst.Rarity == rarity {
			filtered = append(filtered, inst)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return &filtered[rand.Intn(len(filtered))]
}

func generatePacket(rarity string) *protocolo.Packet {
	instrument := GetRandomInstrumentByRarity(rarity)
	if instrument == nil {
		return nil
	}
	return &protocolo.Packet{
		ID:         randomGenerate(4),
		Rarity:     rarity,
		Instrument: *instrument,
		Opened:     false,
	}
}

func initializePacketStock() {
	packetStock = make(map[string]*protocolo.Packet)
	rarities := []string{"Comum", "Raro", "Épico", "Lendário"}
	for _, rarity := range rarities {
		for i := 0; i < 50; i++ {
			packet := generatePacket(rarity)
			if packet != nil {
				packetStock[packet.ID] = packet
			}
		}
	}
	logger.Printf("Estoque de pacotes inicializado com %d pacotes.", len(packetStock))
}

func openPacket(player *User) {
	if raftNode.State() != raft.Leader {
		leaderAddr := string(raftNode.Leader())
		sendScreenMsg(player.Conn, fmt.Sprintf("Erro: Ação negada. O servidor líder é %s. Tente novamente.", leaderAddr))
		logger.Printf(ColorYellow+"Requisição de compra recebida por um não-líder. O líder atual é: %s"+ColorReset, leaderAddr)
		return
	}

	cmd := []byte(fmt.Sprintf("COMPRAR_PACOTE:%s", player.Login))
	future := raftNode.Apply(cmd, 500*time.Millisecond)
	if err := future.Error(); err != nil {
		logger.Printf(ColorRed+"Erro ao aplicar comando RAFT: %v"+ColorReset, err)
		sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "RAFT_ERROR"}})
		return
	}

	logger.Printf("Comando de compra para %s foi comitado no cluster RAFT.", player.Login)

	stockMu.Lock()
	defer stockMu.Unlock()
	if player.Moedas < 20 {
		sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "NO_BALANCE"}})
		return
	}
	for id, packet := range packetStock {
		if !packet.Opened {
			player.Moedas -= 20
			packet.Opened = true
			newInstrument := packet.Instrument
			player.Inventario.Instrumentos = append(player.Inventario.Instrumentos, newInstrument)
			delete(packetStock, id)

			newPkt := generatePacket(packet.Rarity)
			if newPkt != nil {
				packetStock[newPkt.ID] = newPkt
			}

			resp := protocolo.CompraResponse{
				Status:          "COMPRA_APROVADA",
				NovoInstrumento: &newInstrument,
				Inventario:      player.Inventario,
			}
			sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: resp})
			return
		}
	}
	sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "EMPTY_STORAGE"}})
}

// --- INTERPRETADOR E CONEXÃO ---

func handleConnection(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	for {
		message, err := reader.ReadString('\n')
		if err != nil {
			mu.Lock()
			player := findPlayerByConn(conn)
			if player != nil {
				player.Online = false
				player.Conn = nil
				logger.Printf(ColorYellow+"Usuário %s desconectou."+ColorReset, player.Login)
			}
			mu.Unlock()
			return
		}
		if !interpreter(conn, message) {
			break
		}
	}
}

func interpreter(conn net.Conn, fullMessage string) bool {
	var msg protocolo.Message
	if err := json.Unmarshal([]byte(fullMessage), &msg); err != nil {
		sendScreenMsg(conn, "Mensagem inválida.")
		return true
	}

	player := findPlayerByConn(conn)

	switch msg.Type {
	case "CADASTRO":
		var data protocolo.SignInRequest
		mapToStruct(msg.Data, &data)
		cadastrarUser(conn, data)
	case "LOGIN":
		var data protocolo.LoginRequest
		mapToStruct(msg.Data, &data)
		loginUser(conn, data)
	case "CREATE_ROOM":
		createRoom(conn)
	case "FIND_ROOM":
		var data protocolo.RoomRequest
		mapToStruct(msg.Data, &data)
		findRoom(conn, data.Mode, "")
	case "PRIV_ROOM":
		var data protocolo.RoomRequest
		mapToStruct(msg.Data, &data)
		findRoom(conn, "", data.RoomCode)
	case "COMPRA":
		if player != nil {
			openPacket(player)
		}
	case "CHECK_BALANCE":
		if player != nil {
			sendJSON(conn, protocolo.Message{Type: "BALANCE_RESPONSE", Data: protocolo.BalanceResponse{Saldo: player.Moedas}})
		}
	case "SELECT_INSTRUMENT":
		var req protocolo.SelectInstrumentRequest
		mapToStruct(msg.Data, &req)
		if player != nil && req.InstrumentoID >= 0 && req.InstrumentoID < len(player.Inventario.Instrumentos) {
			player.SelectedInstrument = &player.Inventario.Instrumentos[req.InstrumentoID]
			sendScreenMsg(conn, fmt.Sprintf("Instrumento '%s' selecionado para a batalha!", player.SelectedInstrument.Name))
		} else {
			sendScreenMsg(conn, "Seleção de instrumento inválida.")
		}
	case "PLAY_NOTE":
		handlePlayNote(conn, msg.Data)
	case "QUIT":
		return false
	default:
		sendScreenMsg(conn, "Comando inválido.")
	}
	return true
}

// --- Servidor HTTP para a API REST ---
func startHttpApi(nodeID string, httpAddr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/join", func(w http.ResponseWriter, r *http.Request) {
		if raftNode.State() != raft.Leader {
			http.Error(w, "Not the leader. Try another node.", http.StatusBadRequest)
			return
		}

		var reqBody map[string]string
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, "Bad request body", http.StatusBadRequest)
			return
		}

		nodeIdToAdd := reqBody["id"]
		nodeAddrToAdd := reqBody["addr"]

		if nodeIdToAdd == "" || nodeAddrToAdd == "" {
			http.Error(w, "Missing node ID or address", http.StatusBadRequest)
			return
		}

		logger.Printf(ColorCyan+"API REST: Recebido pedido para adicionar o nó %s com endereço %s"+ColorReset, nodeIdToAdd, nodeAddrToAdd)

		future := raftNode.AddVoter(raft.ServerID(nodeIdToAdd), raft.ServerAddress(nodeAddrToAdd), 0, 0)
		if err := future.Error(); err != nil {
			logger.Printf(ColorRed+"Erro ao adicionar nó ao cluster: %v"+ColorReset, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		logger.Printf(ColorGreen+"Nó %s adicionado ao cluster com sucesso!"+ColorReset, nodeIdToAdd)
		w.WriteHeader(http.StatusOK)
	})

	logger.Printf(ColorYellow+"API REST para gerenciamento do cluster escutando em %s"+ColorReset, httpAddr)
	if err := http.ListenAndServe(httpAddr, mux); err != nil {
		logger.Fatalf(ColorRed+"Falha ao iniciar servidor HTTP da API: %v"+ColorReset, err)
	}
}

// --- FUNÇÃO MAIN ---
func main() {
	nodeID := flag.String("id", "", "ID do nó")
	tcpAddr := flag.String("tcp_addr", ":8080", "Endereço TCP para clientes")
	raftAddr := flag.String("raft_addr", ":12000", "Endereço para comunicação RAFT")
	httpAddr := flag.String("http_addr", ":8090", "Endereço para a API REST")
	joinAddrs := flag.String("join_addrs", "", "Lista de endereços de nós para se juntar")
	// A flag -bootstrap foi removida para usarmos a lógica dinâmica.
	flag.Parse()

	if *nodeID == "" {
		log.Fatal("O ID do nó é obrigatório. Use a flag -id.")
	}

	logger = log.New(os.Stdout, fmt.Sprintf("[%s] ", *nodeID), log.Ltime|log.Lmicroseconds)
	logger.Printf(ColorYellow + "Iniciando servidor..." + ColorReset)

	rand.Seed(time.Now().UnixNano())
	os.MkdirAll("data", 0755)
	loadPlayerData()
	if err := loadInstruments(); err != nil {
		logger.Fatal(err)
	}
	initializePacketStock()

	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(*nodeID)
	config.LogLevel = "ERROR"

	raftDataDir := filepath.Join("data", *nodeID)
	os.MkdirAll(raftDataDir, 0700)

	logStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDataDir, "raft-log.db"))
	if err != nil {
		logger.Fatalf("Erro ao criar log store: %s", err)
	}
	stableStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDataDir, "raft-stable.db"))
	if err != nil {
		logger.Fatalf("Erro ao criar stable store: %s", err)
	}
	snapshotStore, err := raft.NewFileSnapshotStore(raftDataDir, 2, os.Stderr)
	if err != nil {
		logger.Fatalf("Erro ao criar snapshot store: %s", err)
	}

	transport, err := raft.NewTCPTransport(*raftAddr, nil, 3, 10*time.Second, os.Stderr)
	if err != nil {
		logger.Fatalf("Erro ao criar transport: %s", err)
	}

	raftNode, err = raft.NewRaft(config, &FSM{}, logStore, stableStore, snapshotStore, transport)
	if err != nil {
		logger.Fatalf("Erro ao criar nó RAFT: %s", err)
	}

	// --- INÍCIO DO BLOCO DE LÓGICA DE CLUSTER CORRIGIDO ---
	// Tentamos nos juntar a um cluster existente primeiro.
	// O timeout de 20s na função joinCluster nos dá tempo suficiente.
	err = joinCluster(*joinAddrs, *nodeID, *raftAddr)
	if err != nil {
		logger.Printf(ColorYellow+"AVISO: Não foi possível se juntar a um cluster existente (%v). Verificando se podemos iniciar um novo."+ColorReset, err)

		// Se a tentativa de join falhou, verificamos se o cluster já foi configurado.
		// Se não tiver servidores, significa que este é um cluster totalmente novo,
		// então este nó tem permissão para ser o primeiro.
		configuration := raft.Configuration{
			Servers: []raft.Server{
				{
					ID:      config.LocalID,
					Address: transport.LocalAddr(),
				},
			},
		}
		bootstrapFuture := raftNode.BootstrapCluster(configuration)
		if err := bootstrapFuture.Error(); err != nil {
			// Se o erro for "cluster already bootstrapped", é normal se outro nó ganhou a corrida.
			// Qualquer outro erro é fatal.
			if err != raft.ErrCantBootstrap {
				logger.Fatalf("Falha ao fazer bootstrap do cluster: %v", err)
			} else {
				logger.Println(ColorYellow + "Outro nó iniciou o cluster primeiro. Continuaremos como seguidor." + ColorReset)
			}
		} else {
			logger.Println(ColorGreen + "Nenhum cluster existente encontrado. Bootstrap realizado. Somos o primeiro nó!" + ColorReset)
		}
	} else {
		logger.Printf(ColorGreen + "SUCESSO: Nó se juntou a um cluster existente!" + ColorReset)
	}
	// --- FIM DO BLOCO DE LÓGICA DE CLUSTER CORRIGIDO ---

	go startHttpApi(*nodeID, *httpAddr)

	go func() {
		for leader := range raftNode.LeaderCh() {
			if leader {
				logger.Println(ColorGreen + "***********************************" + ColorReset)
				logger.Println(ColorGreen + "*** EU SOU O LÍDER DO CLUSTER ***" + ColorReset)
				logger.Println(ColorGreen + "***********************************" + ColorReset)
			} else {
				logger.Println(ColorYellow + "--- Liderança perdida, agora sou um seguidor ---" + ColorReset)
			}
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		savePlayerData()
		os.Exit(0)
	}()

	listener, err := net.Listen("tcp", *tcpAddr)
	if err != nil {
		logger.Fatalf("Erro ao iniciar o servidor TCP: %s", err)
	}
	defer listener.Close()
	logger.Printf(ColorYellow+"Servidor TCP para clientes iniciado em %s"+ColorReset, *tcpAddr)

	salas = make(map[string]*Sala)
	salasEmEspera = make([]*Sala, 0)
	playersInRoom = make(map[string]*Sala)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleConnection(conn)
	}
}

// --- FUNÇÃO JOINCLUSTER ---
func joinCluster(joinAddrs, nodeID, raftAddr string) error {
	if joinAddrs == "" {
		// Se nenhum endereço de join for fornecido, não é um erro,
		// apenas significa que devemos tentar ser o primeiro nó.
		return fmt.Errorf("nenhum endereço de join fornecido")
	}

	peerList := strings.Split(joinAddrs, ",")

	// Tenta por um tempo mais curto, para não demorar tanto para decidir ser o líder
	for i := 0; i < 5; i++ {
		for _, peer := range peerList {
			logger.Printf(ColorCyan+"Tentando se juntar ao cluster via nó: %s"+ColorReset, peer)
			b, err := json.Marshal(map[string]string{"id": nodeID, "addr": raftAddr})
			if err != nil {
				continue // Ignora erros de marshal
			}

			client := http.Client{Timeout: 2 * time.Second}
			resp, err := client.Post(fmt.Sprintf("http://%s/join", peer), "application/json", bytes.NewReader(b))
			if err != nil {
				// Erros de conexão são esperados se os outros nós não subiram ainda
				// logger.Printf(ColorRed+"Erro ao contatar peer %s: %v"+ColorReset, peer, err)
				continue
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				logger.Printf(ColorGreen+"SUCESSO: Juntou-se ao cluster via nó %s!"+ColorReset, peer)
				return nil // Sucesso!
			}
			// Não loga mais falhas para não poluir o terminal
			// logger.Printf(ColorYellow+"Falha ao se juntar via peer %s. Status: %s"+ColorReset, peer, resp.Status)
		}
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("não foi possível se juntar ao cluster após várias tentativas")
}
