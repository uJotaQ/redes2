package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"pbl_redes/protocolo"
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
	CurrentTurn  int // 1 para Jogador1, 2 para Jogador2
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
	mqttClient         mqtt.Client // Cliente MQTT Global
)

const playerDataFile = "data/players.json"
const instrumentDataFile = "data/instrumentos.json"

// --- LÓGICA DE PUBLICAÇÃO MQTT ---

// publishToPlayer envia uma mensagem para o tópico específico de um jogador.
func publishToPlayer(salaID, login string, message protocolo.Message) {
	if mqttClient == nil || !mqttClient.IsConnected() {
		fmt.Println("MQTT client não está conectado, pulando publicação.")
		return
	}
	topic := fmt.Sprintf("game/%s/%s", salaID, login)
	payload, _ := json.Marshal(message)
	token := mqttClient.Publish(topic, 0, false, payload)
	go func() {
		_ = token.Wait() // Espera a publicação completar, mas não bloqueia
		if token.Error() != nil {
			fmt.Printf("Erro ao publicar no tópico %s: %v\n", topic, token.Error())
		}
	}()
}

// --- PERSISTÊNCIA DE DADOS ---

func loadPlayerData() {
	mu.Lock()
	defer mu.Unlock()
	data, err := os.ReadFile(playerDataFile)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("Arquivo de jogadores (%s) não encontrado. Um novo será criado.\n", playerDataFile)
			players = make(map[string]*User)
		}
		return
	}
	json.Unmarshal(data, &players)
	fmt.Printf("%d jogadores carregados.\n", len(players))
}

func savePlayerData() {
	fmt.Println("\nSalvando dados dos jogadores...")
	mu.Lock()
	defer mu.Unlock()
	for _, player := range players {
		player.Online = false
		player.Conn = nil
	}
	data, _ := json.MarshalIndent(players, "", "  ")
	os.WriteFile(playerDataFile, data, 0644)
	fmt.Printf("Dados de %d jogadores salvos.\n", len(players))
}

// --- LÓGICA DE LOGIN E CADASTRO (Mantida via TCP para resposta direta) ---

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
	// Informa ao cliente o ID da sala para ele se inscrever no tópico MQTT
	// (Neste ponto ainda não há sala, mas essa é a ideia para o futuro)
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

// --- FUNÇÕES AUXILIARES (Comunicação TCP direta mantida para mensagens simples) ---

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
			// Envia o pareamento via TCP para ambos os jogadores.
			// Após isso, a comunicação do jogo será via MQTT.
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

// --- LÓGICA DO JOGO MUSICAL (MODIFICADA PARA MQTT) ---

func startGame(sala *Sala) {
	p1 := findPlayerByConn(sala.Jogador1)
	p2 := findPlayerByConn(sala.Jogador2)
	if p1 == nil || p2 == nil {
		return
	}

	sala.Game = &GameState{
		CurrentTurn: 1,
	}

	// Publica a mensagem de início de jogo para cada jogador em seu tópico
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

	// Publica a atualização de turno para cada jogador
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

	// Publica o resultado do round para cada jogador
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

	// Define o próximo turno
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

	// Publica a mensagem de fim de jogo para cada jogador
	gameOverMsgP1 := protocolo.GameOverMessage{
		Winner:       winner,
		FinalScoreP1: game.Player1Score,
		FinalScoreP2: game.Player2Score,
		CoinsEarned:  game.Player1Score*5 + (func() int {
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
		CoinsEarned:  game.Player2Score*5 + (func() int {
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
    fmt.Printf("Carregados %d instrumentos da base de dados.\n", len(instrumentDatabase))
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
	fmt.Printf("Estoque de pacotes inicializado com %d pacotes.\n", len(packetStock))
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

func openPacket(player *User) {
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
				Status:        "COMPRA_APROVADA",
				NovoInstrumento: &newInstrument,
				Inventario:    player.Inventario,
			}
			sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: resp})
			return
		}
	}
	sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "EMPTY_STORAGE"}})
}

// --- INTERPRETADOR DE MENSAGENS E MAIN LOOP ---

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
				fmt.Printf("Usuário %s desconectou.\n", player.Login)
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

func main() {
	rand.Seed(time.Now().UnixNano())
	os.Mkdir("data", 0755)

	// --- Conexão MQTT com retentativas e failover ---
	brokerAddresses := []string{
		"tcp://broker1:1883",
		"tcp://broker2:1883",
		"tcp://broker3:1883",
	}
	opts := mqtt.NewClientOptions()
	for _, addr := range brokerAddresses {
		opts.AddBroker(addr)
	}
	opts.SetClientID("game-server-main")
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)

	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.WaitTimeout(5*time.Second) && token.Error() != nil {
		fmt.Println("Não foi possível conectar a nenhum broker MQTT. Encerrando.")
		os.Exit(1)
	}
	fmt.Println("Conectado ao Broker MQTT com sucesso.")
	// --- Fim da Conexão MQTT ---

	loadPlayerData()
	if err := loadInstruments(); err != nil {
		fmt.Println(err)
		return
	}
	initializePacketStock()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		savePlayerData()
		mqttClient.Disconnect(250) // Desconecta o cliente MQTT graciosamente
		os.Exit(0)
	}()

	listener, err := net.Listen("tcp", ":8080")
	if err != nil {
		fmt.Println("Erro ao iniciar o servidor:", err)
		return
	}
	defer listener.Close()
	fmt.Println("Servidor TCP iniciado na porta 8080. Pressione Ctrl+C para salvar e fechar.")

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