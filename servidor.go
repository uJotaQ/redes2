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

// Estrutura para encapsular a resposta da FSM
type fsmApplyResponse struct { // Resposta da FSM pra o servidor dela proprio (tipo confirmacao de que o processo deu certo ou nao)
	response interface{}
	err      error
}

type FsmTradeCompletionResult struct {
	P1Login       string                   
    P2Login       string
	ResponseForP1 *protocolo.TradeResponse // Resposta formatada para o Player 1 original
	ResponseForP2 *protocolo.TradeResponse // Resposta formatada para o Player 2 que completou
}

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

// Struct para armazenar ofertas de troca pendentes
type TradeOffer struct {
	Player1     string
	Player2     string
	CardPlayer1 protocolo.Instrumento
	Finished    bool // Talvez remover isso dps
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
	fsmRand            *rand.Rand

	tradeMu      sync.Mutex

	transport    *raft.NetworkTransport

	activeTrades map[string]*TradeOffer
)

// --- RAFT FSM (FINITE STATE MACHINE) ---
type FSM struct{}

func (f *FSM) Apply(log *raft.Log) interface{} {
	cmd := string(log.Data)
	// Divide o comando em "TIPO" e "PAYLOAD"
	// Usamos SplitN com N=2 para garantir que o payload (que pode ser JSON) não seja quebrado
	parts := strings.SplitN(cmd, ":", 2)

	if len(parts) < 1 {
		return fsmApplyResponse{err: fmt.Errorf("comando FSM vazio")}
	}

	cmdType := parts[0]
	cmdPayload := ""
	if len(parts) > 1 {
		cmdPayload = parts[1]
	}

	// Um switch para lidar com diferentes tipos de comando
	switch cmdType {

	case "CADASTRAR_JOGADOR":
		var data protocolo.SignInRequest
		if err := json.Unmarshal([]byte(cmdPayload), &data); err != nil {
			return fsmApplyResponse{err: fmt.Errorf("FSM: falha ao decodificar cadastro")}
		}

		// Esta é a lógica que estava em cadastrarUser, agora dentro da FSM
		mu.Lock()
		defer mu.Unlock()

		if _, exists := players[data.Login]; exists {
			return fsmApplyResponse{err: fmt.Errorf("Login já existe.")}
		}

		players[data.Login] = &User{
			Login:  data.Login,
			Senha:  data.Senha,
			Moedas: 100, // Saldo inicial
		}
		logger.Printf("[FSM] Jogador %s cadastrado com sucesso.", data.Login)
		return fsmApplyResponse{response: "OK"} // Sucesso

	case "COMPRAR_PACOTE":
		playerLogin := cmdPayload // Aqui o payload é só o login

		// --- Início da Lógica de Compra ---
		mu.Lock() // Bloqueia o mapa de players
		player, ok := players[playerLogin]
		if !ok {
			mu.Unlock()
			logger.Printf("[FSM] Erro: jogador %s não encontrado.", playerLogin)
			// Este era o seu erro. Agora ele só acontece se o jogador for deletado.
			return fsmApplyResponse{err: fmt.Errorf("jogador não encontrado")}
		}
		mu.Unlock() // Desbloqueia o mapa de players

		stockMu.Lock() // Bloqueia o estoque
		defer stockMu.Unlock()

		if player.Moedas < 20 {
			// A checagem de saldo autoritativa (final)
			return fsmApplyResponse{response: protocolo.CompraResponse{Status: "NO_BALANCE"}}
		}

		for id, packet := range packetStock {
			if !packet.Opened {
				player.Moedas -= 20
				packet.Opened = true
				newInstrument := packet.Instrument
				player.Inventario.Instrumentos = append(player.Inventario.Instrumentos, newInstrument)
				delete(packetStock, id)

				// Reabastece o estoque
				newPkt := generatePacket(fsmRand, packet.Rarity) // Usa o fsmRand
				if newPkt != nil {
					packetStock[newPkt.ID] = newPkt
				}

				resp := protocolo.CompraResponse{
					Status:          "COMPRA_APROVADA",
					NovoInstrumento: &newInstrument,
					Inventario:      player.Inventario,
				}
				return fsmApplyResponse{response: resp}
			}
		}
		// Se o loop terminar, não havia estoque
		return fsmApplyResponse{response: protocolo.CompraResponse{Status: "EMPTY_STORAGE"}}
		
	case "PROPOSE_TRADE":
        // O payload aqui é um JSON de uma struct interna que definimos
        var req struct {
            Player1Login string
            Player2Login string
            CardPlayer1  protocolo.Instrumento
        }
        if err := json.Unmarshal([]byte(cmdPayload), &req); err != nil {
            return fsmApplyResponse{err: fmt.Errorf("FSM: falha ao decodificar troca")}
        }

        // 1. Travar ambos os mapas
        mu.Lock()
        tradeMu.Lock()
        defer mu.Unlock()
        defer tradeMu.Unlock()

        // 2. Validações (feitas aqui dentro da FSM para garantir consistência)
        if req.Player1Login == req.Player2Login {
            return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "Você não pode trocar cartas consigo mesmo."}}
        }

        player1, ok1 := players[req.Player1Login]
        player2, ok2 := players[req.Player2Login]
        if !ok1 || !ok2 {
            return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "Jogador não encontrado."}}
        }

        // 3. Verificar se o P1 realmente tem a carta
        cardIndexP1 := -1
        for i, card := range player1.Inventario.Instrumentos {
            if card.ID == req.CardPlayer1.ID { // Compara por ID
                cardIndexP1 = i
                break
            }
        }
        if cardIndexP1 == -1 {
            return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "Você não possui mais esta carta."}}
        }

        // 4. Lógica de "Mão Dupla"
        reverseTradeKey := req.Player2Login + ":" + req.Player1Login
        reverseOffer, exists := activeTrades[reverseTradeKey]

        if exists {
            // --- TROCA COMPLETA ---
            // A outra parte (P2) já ofereceu. Vamos completar a troca.
            
            // A. Pegar a carta do P2 da oferta existente
            cardP2 := reverseOffer.CardPlayer1 // P1 da oferta reversa é o P2
            
            // B. Verificar se P2 ainda tem a carta
            cardIndexP2 := -1
            for i, card := range player2.Inventario.Instrumentos {
                if card.ID == cardP2.ID {
                    cardIndexP2 = i
                    break
                }
            }
            if cardIndexP2 == -1 {
                // P2 não tem mais a carta. A oferta dele é inválida.
                delete(activeTrades, reverseTradeKey) // Remove a oferta inválida
                return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "O outro jogador não possui mais a carta oferecida. A oferta dele foi cancelada."}}
            }

            // C. Remover as cartas dos inventários
            // (Remover P1)
            cardP1 := player1.Inventario.Instrumentos[cardIndexP1]
            player1.Inventario.Instrumentos = append(player1.Inventario.Instrumentos[:cardIndexP1], player1.Inventario.Instrumentos[cardIndexP1+1:]...)
            // (Remover P2)
            player2.Inventario.Instrumentos = append(player2.Inventario.Instrumentos[:cardIndexP2], player2.Inventario.Instrumentos[cardIndexP2+1:]...)

            // D. Adicionar as cartas trocadas
            player1.Inventario.Instrumentos = append(player1.Inventario.Instrumentos, cardP2)
            player2.Inventario.Instrumentos = append(player2.Inventario.Instrumentos, cardP1)
            
            // E. Limpar a oferta
            delete(activeTrades, reverseTradeKey)

			respP1 := protocolo.TradeResponse{
                Status:          "TRADE_COMPLETED",
                Message:         fmt.Sprintf("Troca com %s concluída! Você recebeu: %s", player2.Login, cardP2.Name),
                Inventario:      player1.Inventario, // Inventário final do P1
                ReceivedCard:    &cardP2,           // Carta que P1 recebeu
                OtherPlayerLogin: player2.Login,    // Quem P1 trocou
            }
            respP2 := protocolo.TradeResponse{
                Status:          "TRADE_COMPLETED",
                Message:         fmt.Sprintf("Troca com %s concluída! Você recebeu: %s", player1.Login, cardP1.Name),
                Inventario:      player2.Inventario, // Inventário final do P2
                ReceivedCard:    &cardP1,           // Carta que P2 recebeu
                OtherPlayerLogin: player1.Login,    // Quem P2 trocou
            }

            logger.Printf("[FSM] Troca concluída entre %s e %s.", player1.Login, player2.Login)

            // Retorna ambas as respostas, empacotadas na nova struct
            return fsmApplyResponse{response: FsmTradeCompletionResult{
                ResponseForP1: &respP1,
                ResponseForP2: &respP2,
            }}

        } else {
            // --- NOVA OFERTA DE TROCA ---
            tradeKey := req.Player1Login + ":" + req.Player2Login
            if _, exists := activeTrades[tradeKey]; exists {
                return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "Você já tem uma oferta pendente para este jogador."}}
            }
            
            // Criar e salvar a nova oferta
            activeTrades[tradeKey] = &TradeOffer{
                Player1:     req.Player1Login,
                Player2:     req.Player2Login,
                CardPlayer1: req.CardPlayer1,
                Finished:    false,
            }
            
            logger.Printf("[FSM] Nova oferta de troca registrada de %s para %s.", req.Player1Login, req.Player2Login)
            
            return fsmApplyResponse{response: protocolo.TradeResponse{
                Status:  "OFFER_SENT",
                Message: fmt.Sprintf("Oferta de troca enviada para %s.", req.Player2Login),
            }}
        }

	default:
		logger.Printf(ColorRed+"[FSM] Comando desconhecido recebido: %s"+ColorReset, cmdType)
		return fsmApplyResponse{err: fmt.Errorf("comando FSM desconhecido: %s", cmdType)}
	}
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

	if raftNode.State() != raft.Leader {
		// --- LÓGICA DO SEGUIDOR ---
		leaderRaftAddr := string(raftNode.Leader())
		if leaderRaftAddr == "" {
			logger.Printf(ColorRed+"Cadastro de %s negado: líder desconhecido."+ColorReset, data.Login)
			sendJSON(conn, protocolo.Message{Type: "CADASTRO_RESPONSE", Data: protocolo.CadastroResponse{Status: "ERRO", Message: "Erro interno: líder indisponível."}})
			return
		}

		leaderHttpAddr := strings.Replace(leaderRaftAddr, ":12000", ":8090", 1)
		logger.Printf(ColorYellow+"Não sou o líder. Encaminhando cadastro de %s para %s"+ColorReset, data.Login, leaderHttpAddr)

		postBody, _ := json.Marshal(data)
		reqURL := fmt.Sprintf("http://%s/request-register", leaderHttpAddr)

		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Post(reqURL, "application/json", bytes.NewBuffer(postBody))

		if err != nil {
			logger.Printf(ColorRed+"Erro ao encaminhar cadastro para o líder: %v"+ColorReset, err)
			sendJSON(conn, protocolo.Message{Type: "CADASTRO_RESPONSE", Data: protocolo.CadastroResponse{Status: "ERRO", Message: "Erro ao contatar o servidor líder."}})
			return
		}
		defer resp.Body.Close()

		// Decodifica a resposta (CadastroResponse) vinda do líder
		var cadastroResp protocolo.CadastroResponse
		if err := json.NewDecoder(resp.Body).Decode(&cadastroResp); err != nil {
			logger.Printf(ColorRed+"Erro ao decodificar resposta de cadastro do líder: %v"+ColorReset, err)
			sendJSON(conn, protocolo.Message{Type: "CADASTRO_RESPONSE", Data: protocolo.CadastroResponse{Status: "ERRO", Message: "Erro ao ler resposta do líder."}})
			return
		}

		// Encaminha a resposta exata do líder para o cliente
		sendJSON(conn, protocolo.Message{Type: "CADASTRO_RESPONSE", Data: cadastroResp})

	} else {
		// --- LÓGICA DO LÍDER ---
		logger.Printf(ColorGreen+"Sou o líder. Processando cadastro de %s via RAFT."+ColorReset, data.Login)

		cmdPayload, err := json.Marshal(data)
		if err != nil {
			logger.Printf(ColorRed+"Erro ao serializar comando de cadastro: %v"+ColorReset, err)
			sendJSON(conn, protocolo.Message{Type: "CADASTRO_RESPONSE", Data: protocolo.CadastroResponse{Status: "ERRO", Message: "Erro interno no servidor (serialize)."}})
			return
		}

		cmd := []byte("CADASTRAR_JOGADOR:" + string(cmdPayload))

		future := raftNode.Apply(cmd, 500*time.Millisecond)
		if err := future.Error(); err != nil {
			logger.Printf(ColorRed+"Erro ao aplicar comando RAFT (cadastro): %v"+ColorReset, err)
			sendJSON(conn, protocolo.Message{Type: "CADASTRO_RESPONSE", Data: protocolo.CadastroResponse{Status: "ERRO", Message: "Erro interno no servidor (RAFT)."}})
			return
		}

		fsmResp := future.Response().(fsmApplyResponse)
		if fsmResp.err != nil {
			// Erro de negócio (ex: "Login já existe")
			logger.Printf(ColorYellow+"Falha no cadastro FSM: %v"+ColorReset, fsmResp.err)
			sendJSON(conn, protocolo.Message{Type: "CADASTRO_RESPONSE", Data: protocolo.CadastroResponse{Status: "LOGIN_EXISTE", Message: fsmResp.err.Error()}})
			return
		}

		// Sucesso
		sendJSON(conn, protocolo.Message{Type: "CADASTRO_RESPONSE", Data: protocolo.CadastroResponse{Status: "OK", Message: "Cadastro realizado com sucesso!"}})
	}
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

func GetRandomInstrumentByRarity(localRand *rand.Rand, rarity string) *protocolo.Instrumento {
	var filtered []protocolo.Instrumento
	for _, inst := range instrumentDatabase {
		if inst.Rarity == rarity {
			filtered = append(filtered, inst)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return &filtered[localRand.Intn(len(filtered))] // Usa o rand passado
}

func generatePacket(localRand *rand.Rand, rarity string) *protocolo.Packet {
	instrument := GetRandomInstrumentByRarity(localRand, rarity)
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
	// Cria um gerador de números aleatórios com uma semente FIXA.
	// Isso garante que TODOS os servidores gerem o MESMO estoque inicial.
	localRand := rand.New(rand.NewSource(12345)) // Semente fixa

	packetStock = make(map[string]*protocolo.Packet)
	rarities := []string{"Comum", "Raro", "Épico", "Lendário"}
	for _, rarity := range rarities {
		for i := 0; i < 50; i++ {
			// GetRandomInstrumentByRarity precisa ser modificado para aceitar o localRand
			instrument := GetRandomInstrumentByRarity(localRand, rarity) // Passa o rand
			if instrument != nil {
				packet := &protocolo.Packet{
					ID:         randomGenerate(4), // Este rand não afeta o estado
					Rarity:     rarity,
					Instrument: *instrument,
					Opened:     false,
				}
				packetStock[packet.ID] = packet
			}
		}
	}
	logger.Printf("Estoque de pacotes inicializado com %d pacotes.", len(packetStock))
}

func openPacket(player *User) {

	if player.Moedas < 20 {
		logger.Printf(ColorYellow+"Pedido de compra de %s negado localmente: saldo insuficiente (%d moedas)."+ColorReset, player.Login, player.Moedas)
		sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "NO_BALANCE"}})
		return // Encerra a função aqui
	}

	if raftNode.State() != raft.Leader {
		// --- LÓGICA DO SEGUIDOR ---
		// Não somos o líder. Devemos encaminhar para o líder via HTTP REST.

		// 1. Descobrir o endereço HTTP do líder
		// Seu docker-compose usa `servidor1:12000` (RAFT) e `servidor1:8090` (HTTP)
		// Vamos assumir essa convenção de portas (trocar 12000 por 8090)
		leaderRaftAddr := string(raftNode.Leader())
		if leaderRaftAddr == "" {
			logger.Printf(ColorRed + "Pedido de compra falhou: líder desconhecido." + ColorReset)
			sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "RAFT_ERROR"}})
			return
		}

		// IMPORTANTE: Isso assume que a porta HTTP está em :8090 e a RAFT em :12000
		// E que ambos usam o mesmo hostname (ex: "servidor1")
		leaderHttpAddr := strings.Replace(leaderRaftAddr, ":12000", ":8090", 1)

		logger.Printf(ColorYellow+"Não sou o líder. Encaminhando pedido de compra de %s para %s"+ColorReset, player.Login, leaderHttpAddr)

		// 2. Preparar a requisição HTTP
		postBody, _ := json.Marshal(map[string]string{
			"playerLogin": player.Login,
		})
		reqURL := fmt.Sprintf("http://%s/request-purchase", leaderHttpAddr)

		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Post(reqURL, "application/json", bytes.NewBuffer(postBody))

		// 3. Lidar com a resposta do líder
		if err != nil {
			logger.Printf(ColorRed+"Erro ao encaminhar pedido para o líder: %v"+ColorReset, err)
			sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "RAFT_ERROR"}})
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			logger.Printf(ColorRed+"Líder retornou erro: %s"+ColorReset, resp.Status)
			sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "RAFT_ERROR"}})
			return
		}

		// 4. Decodificar a resposta (CompraResponse) e enviar ao cliente
		var compraResp protocolo.CompraResponse
		if err := json.NewDecoder(resp.Body).Decode(&compraResp); err != nil {
			logger.Printf(ColorRed+"Erro ao decodificar resposta do líder: %v"+ColorReset, err)
			sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "RAFT_ERROR"}})
			return
		}

		// Sucesso! Envia a resposta do líder diretamente para o cliente.
		sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: compraResp})

		// Nota: O estado local do seguidor será atualizado em breve,
		// quando o FSM.Apply for executado com o comando vindo do líder.

	} else {
		// --- LÓGICA DO LÍDER ---
		// Somos o líder. Aplicamos o comando no RAFT.
		logger.Printf(ColorGreen+"Sou o líder. Processando pedido de compra de %s via RAFT."+ColorReset, player.Login)

		cmd := []byte(fmt.Sprintf("COMPRAR_PACOTE:%s", player.Login))
		future := raftNode.Apply(cmd, 500*time.Millisecond)
		if err := future.Error(); err != nil {
			logger.Printf(ColorRed+"Erro ao aplicar comando RAFT: %v"+ColorReset, err)
			sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "RAFT_ERROR"}})
			return
		}

		// Pega a resposta que a *nossa própria FSM* retornou
		fsmResp := future.Response().(fsmApplyResponse)
		if fsmResp.err != nil {
			// Isso não deveria acontecer se o comando estiver correto
			logger.Printf(ColorRed+"Erro na FSM: %v"+ColorReset, fsmResp.err)
			sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: protocolo.CompraResponse{Status: "RAFT_ERROR"}})
			return
		}

		// Envia a resposta da FSM (CompraResponse) para o cliente
		sendJSON(player.Conn, protocolo.Message{Type: "COMPRA_RESPONSE", Data: fsmResp.response})
	}
}

func proposeTrade(player *User, data interface{}) {
    var req protocolo.TradeRequest
    if err := mapToStruct(data, &req); err != nil {
        sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: protocolo.TradeResponse{Status: "ERRO", Message: "Requisição inválida."}})
        return
    }

    // Validação local: O índice da carta é válido?
    if req.InstrumentIndex < 0 || req.InstrumentIndex >= len(player.Inventario.Instrumentos) {
        sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: protocolo.TradeResponse{Status: "ERRO", Message: "Seleção de instrumento inválida."}})
        return
    }
    // Pega a carta que o P1 quer trocar
    cardToTrade := player.Inventario.Instrumentos[req.InstrumentIndex]

    // Prepara o payload interno para o RAFT
    // A FSM receberá este payload, não o 'protocolo.TradeRequest'
    internalPayload := map[string]interface{}{
        "Player1Login": player.Login,
        "Player2Login": req.Player2Login,
        "CardPlayer1":  cardToTrade,
    }

    if raftNode.State() != raft.Leader {
        // --- LÓGICA DO SEGUIDOR ---
        leaderRaftAddr := string(raftNode.Leader())
        if leaderRaftAddr == "" {
            sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: protocolo.TradeResponse{Status: "ERRO", Message: "Erro interno: líder indisponível."}})
            return
        }

        leaderHttpAddr := strings.Replace(leaderRaftAddr, ":12000", ":8090", 1)
        logger.Printf(ColorYellow+"Não sou o líder. Encaminhando proposta de troca de %s para %s"+ColorReset, player.Login, leaderHttpAddr)

        postBody, _ := json.Marshal(internalPayload) // Envia o payload interno
        reqURL := fmt.Sprintf("http://%s/request-trade", leaderHttpAddr)

        client := http.Client{Timeout: 3 * time.Second}
        resp, err := client.Post(reqURL, "application/json", bytes.NewBuffer(postBody))

        if err != nil {
            sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: protocolo.TradeResponse{Status: "ERRO", Message: "Erro ao contatar o servidor líder."}})
            return
        }
        defer resp.Body.Close()

        var tradeResp protocolo.TradeResponse
        if err := json.NewDecoder(resp.Body).Decode(&tradeResp); err != nil {
            sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: protocolo.TradeResponse{Status: "ERRO", Message: "Erro ao ler resposta do líder."}})
            return
        }

        // Se a troca foi completa, o inventário local do SEGUIDOR está
        // desatualizado. A FSM vai atualizar, mas a resposta imediata
        // do líder já contém o inventário correto.
        if tradeResp.Status == "TRADE_COMPLETED" {
             player.Inventario = tradeResp.Inventario
        }
        sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: tradeResp})

    } else {
        // --- LÓGICA DO LÍDER ---
        logger.Printf(ColorGreen+"Sou o líder. Processando proposta de troca de %s via RAFT."+ColorReset, player.Login)

        cmdPayload, _ := json.Marshal(internalPayload)
        cmd := []byte("PROPOSE_TRADE:" + string(cmdPayload))

        future := raftNode.Apply(cmd, 500*time.Millisecond)
        if err := future.Error(); err != nil {
            sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: protocolo.TradeResponse{Status: "ERRO", Message: "Erro interno no servidor (RAFT)."}})
            return
        }

        fsmResp := future.Response().(fsmApplyResponse)
        if fsmResp.err != nil {
             // Erro da FSM (ex: falha ao decodificar)
            sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: protocolo.TradeResponse{Status: "ERRO", Message: fsmResp.err.Error()}})
            return
        }

        // --- MUDANÇA AQUI: Verifica o tipo de resposta da FSM ---
        switch result := fsmResp.response.(type) {
        case protocolo.TradeResponse: // Caso: Oferta enviada ou Erro único
            tradeResp := result
            // Envia a resposta normal para o iniciador
            sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: tradeResp})

        case FsmTradeCompletionResult: // Caso: Troca foi completada!
            completionResult := result

            // Descobre qual resposta é para quem
            var responseForCaller *protocolo.TradeResponse // Quem chamou esta função (ex: Sa)
            var responseForOther *protocolo.TradeResponse  // O outro jogador (ex: Leo)
            var otherPlayerLogin string

            // 'player' aqui é quem iniciou ESTE comando PROPOSE_TRADE
            // Verifica usando os logins retornados pela FSM
            if player.Login == completionResult.P2Login { // player é P2?
                responseForCaller = completionResult.ResponseForP2
                responseForOther = completionResult.ResponseForP1
                otherPlayerLogin = completionResult.P1Login
            } else { // player é P1?
                responseForCaller = completionResult.ResponseForP1
                responseForOther = completionResult.ResponseForP2
                otherPlayerLogin = completionResult.P2Login
            }

            // Atualiza o inventário local do CHAMADOR (quem iniciou este comando)
            player.Inventario = responseForCaller.Inventario
            // Envia a resposta para o CHAMADOR
            sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: *responseForCaller})

            // --- CORREÇÃO: Faz BROADCAST para notificar o OUTRO jogador ---
            messageForOther := protocolo.Message{Type: "TRADE_RESPONSE", Data: *responseForOther}
            broadcastNotificationToPlayer(otherPlayerLogin, messageForOther)
            // --- FIM DA CORREÇÃO ---

        default:
             // Erro inesperado
            sendJSON(player.Conn, protocolo.Message{Type: "TRADE_RESPONSE", Data: protocolo.TradeResponse{Status: "ERRO", Message: "Resposta interna do servidor inválida."}})
        }
	}
}

// broadcastNotificationToPlayer envia uma notificação para todos os outros nós do cluster.
func broadcastNotificationToPlayer(targetLogin string, messageToSend protocolo.Message) {
    if raftNode.State() != raft.Leader {
        // Apenas o líder faz broadcast
        return
    }

    // Pega a configuração atual do cluster
    configFuture := raftNode.GetConfiguration()
    if err := configFuture.Error(); err != nil {
        logger.Printf(ColorRed+"Erro ao obter configuração do cluster para broadcast: %v"+ColorReset, err)
        return
    }
    config := configFuture.Configuration()

    // Prepara o corpo da notificação
    notificationPayload := map[string]interface{}{
        "target_login": targetLogin,
        "message":      messageToSend,
    }
    jsonBody, _ := json.Marshal(notificationPayload)

    // Envia para cada servidor no cluster (exceto ele mesmo)
    for _, server := range config.Servers {
        if server.Address == transport.LocalAddr() {
            continue // Não envia para si mesmo
        }

        // Converte o endereço RAFT (ex: "servidor2:12000") para o endereço HTTP (ex: "servidor2:8090")
        httpAddr := strings.Replace(string(server.Address), ":12000", ":8090", 1)
        targetURL := fmt.Sprintf("http://%s/notify-player", httpAddr)

        // Dispara a requisição em uma goroutine para não bloquear
        go func(url string, body []byte) {
            client := http.Client{Timeout: 2 * time.Second}
            resp, err := client.Post(url, "application/json", bytes.NewReader(body))
            if err != nil {
                // É normal falhar se um nó estiver offline
                // logger.Printf(ColorYellow+"Falha ao enviar notificação para %s: %v"+ColorReset, url, err)
                return
            }
            defer resp.Body.Close()
            // Não precisamos checar a resposta, apenas tentamos enviar
             logger.Printf(ColorCyan+"Broadcast de notificação para %s (via %s) enviado."+ColorReset, targetLogin, url)
        }(targetURL, jsonBody)
    }
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

	case "GET_INVENTORY":
        if player != nil {
            // Acessa o inventário (protegido pelo mutex global 'mu')
            // Usamos Lock/Unlock para leitura, permitindo leituras concorrentes
            mu.Lock() // Usar Lock normal por enquanto para segurança, já que outros lugares usam Lock
            currentInv := player.Inventario
            mu.Unlock()

            // Envia o inventário atual para o cliente
            sendJSON(conn, protocolo.Message{
                Type: "INVENTORY_RESPONSE",
                Data: protocolo.InventoryResponse{Inventario: currentInv},
            })
        }

	case "PROPOSE_TRADE":
		if player != nil {
			proposeTrade(player, msg.Data)
		}

	case "QUIT":
		if player != nil {
			mu.Lock()
			player.Online = false
			player.Conn = nil // Limpa a referência da conexão
			logger.Printf(ColorYellow+"Usuário %s desconectou (QUIT)."+ColorReset, player.Login)
			mu.Unlock()
		}
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

	mux.HandleFunc("/request-purchase", func(w http.ResponseWriter, r *http.Request) {
		// Este endpoint SÓ deve ser chamado no LÍDER
		if raftNode.State() != raft.Leader {
			http.Error(w, "Não sou o líder. Encaminhe para "+string(raftNode.Leader()), http.StatusServiceUnavailable)
			return
		}

		// Decodifica o login do jogador vindo do seguidor
		var reqBody map[string]string
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
			return
		}
		playerLogin := reqBody["playerLogin"]
		if playerLogin == "" {
			http.Error(w, "Missing playerLogin", http.StatusBadRequest)
			return
		}

		logger.Printf(ColorCyan+"[API REST] Recebido pedido de compra do jogador %s (encaminhado por um seguidor)"+ColorReset, playerLogin)

		// Aplica o comando no RAFT (exatamente como o líder faria)
		cmd := []byte(fmt.Sprintf("COMPRAR_PACOTE:%s", playerLogin))
		future := raftNode.Apply(cmd, 500*time.Millisecond)
		if err := future.Error(); err != nil {
			logger.Printf(ColorRed+"[API REST] Erro ao aplicar comando RAFT: %v"+ColorReset, err)
			http.Error(w, "Erro no RAFT", http.StatusInternalServerError)
			return
		}

		// Pega a resposta da FSM
		fsmResp := future.Response().(fsmApplyResponse)
		if fsmResp.err != nil {
			logger.Printf(ColorRed+"[API REST] Erro na FSM: %v"+ColorReset, fsmResp.err)
			http.Error(w, "Erro na FSM", http.StatusInternalServerError)
			return
		}

		// Envia a CompraResponse (em JSON) de volta para o servidor seguidor
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fsmResp.response)
	})

	mux.HandleFunc("/request-register", func(w http.ResponseWriter, r *http.Request) {
		if raftNode.State() != raft.Leader {
			http.Error(w, "Não sou o líder.", http.StatusServiceUnavailable)
			return
		}

		var reqBody protocolo.SignInRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
			return
		}

		logger.Printf(ColorCyan+"[API REST] Recebido pedido de cadastro para %s (encaminhado por um seguidor)"+ColorReset, reqBody.Login)

		cmdPayload, _ := json.Marshal(reqBody)
		cmd := []byte("CADASTRAR_JOGADOR:" + string(cmdPayload))

		future := raftNode.Apply(cmd, 500*time.Millisecond)
		if err := future.Error(); err != nil {
			logger.Printf(ColorRed+"[API REST] Erro ao aplicar comando RAFT (cadastro): %v"+ColorReset, err)
			resp := protocolo.CadastroResponse{Status: "ERRO", Message: "Erro interno no servidor (RAFT)."}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(resp)
			return
		}

		fsmResp := future.Response().(fsmApplyResponse)
		w.Header().Set("Content-Type", "application/json")
		if fsmResp.err != nil {
			// Erro de negócio (ex: "Login já existe")
			logger.Printf(ColorRed+"[API REST] Falha no cadastro FSM: %v"+ColorReset, fsmResp.err)
			resp := protocolo.CadastroResponse{Status: "LOGIN_EXISTE", Message: fsmResp.err.Error()}
			w.WriteHeader(http.StatusBadRequest) // Envia o erro como resposta HTTP
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Sucesso
		resp := protocolo.CadastroResponse{Status: "OK", Message: "Cadastro realizado com sucesso!"}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/request-trade", func(w http.ResponseWriter, r *http.Request) {
        if raftNode.State() != raft.Leader {
            http.Error(w, "Não sou o líder.", http.StatusServiceUnavailable)
            return
        }

        // Decodifica o payload INTERNO (reqBody é declarado aqui)
        var reqBody map[string]interface{}
        if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
            http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
            return
        }

        logger.Printf(ColorCyan+"[API REST] Recebido pedido de troca (encaminhado por um seguidor)"+ColorReset)

        cmdPayload, _ := json.Marshal(reqBody)
        cmd := []byte("PROPOSE_TRADE:" + string(cmdPayload))

        future := raftNode.Apply(cmd, 500*time.Millisecond)
        if err := future.Error(); err != nil {
            resp := protocolo.TradeResponse{Status: "ERRO", Message: "Erro interno no servidor (RAFT)."}
            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusInternalServerError)
            json.NewEncoder(w).Encode(resp)
            return
        }

        fsmResp := future.Response().(fsmApplyResponse)
        w.Header().Set("Content-Type", "application/json")
        if fsmResp.err != nil {
            resp := protocolo.TradeResponse{Status: "ERRO", Message: fsmResp.err.Error()}
            w.WriteHeader(http.StatusBadRequest)
            json.NewEncoder(w).Encode(resp)
            return
        }

        var httpResponse protocolo.TradeResponse // A resposta para o SEGUIDOR que pediu

        switch result := fsmResp.response.(type) {
        case protocolo.TradeResponse: // Oferta enviada ou Erro único
            httpResponse = result

        case FsmTradeCompletionResult: // Troca completada!
            completionResult := result

            // Descobre quem iniciou a requisição HTTP (o P1 da struct interna)
            initiatorLogin := ""
            if login, ok := reqBody["Player1Login"].(string); ok {
                initiatorLogin = login
            }

            // Descobre qual resposta enviar de volta para o Seguidor
            var responseForOther *protocolo.TradeResponse
            var otherPlayerLogin string

            // Verifica usando os logins retornados pela FSM
            if initiatorLogin == completionResult.P2Login { // Iniciador é P2?
                httpResponse = *completionResult.ResponseForP2
                responseForOther = completionResult.ResponseForP1
                otherPlayerLogin = completionResult.P1Login
            } else { // Iniciador é P1?
                httpResponse = *completionResult.ResponseForP1
                responseForOther = completionResult.ResponseForP2
                otherPlayerLogin = completionResult.P2Login
            }

            // --- CORREÇÃO: Faz BROADCAST para notificar o OUTRO jogador ---
            messageForOther := protocolo.Message{Type: "TRADE_RESPONSE", Data: *responseForOther}
            broadcastNotificationToPlayer(otherPlayerLogin, messageForOther)
            // --- FIM DA CORREÇÃO ---

        default:
            // Erro inesperado
            httpResponse = protocolo.TradeResponse{Status: "ERRO", Message: "Resposta interna do servidor inválida."}
            w.WriteHeader(http.StatusInternalServerError)
            json.NewEncoder(w).Encode(httpResponse)
            return
        }

        // Envia a resposta relevante de volta para o Seguidor via HTTP
        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(httpResponse)
    }) // <-- Fim do handler /request-trade

	mux.HandleFunc("/notify-player", func(w http.ResponseWriter, r *http.Request) {
        // Estrutura esperada no corpo da requisição POST
        var notification struct {
            TargetLogin string                 `json:"target_login"`
            Message     protocolo.Message      `json:"message"` // A mensagem completa a ser enviada
        }

        if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
            http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
            return
        }

        if notification.TargetLogin == "" {
             http.Error(w, "Missing target_login", http.StatusBadRequest)
             return
        }

        logger.Printf(ColorCyan+"[API REST] Recebido pedido para notificar %s"+ColorReset, notification.TargetLogin)

        // Tenta encontrar o jogador localmente e enviar a mensagem
        mu.Lock()
        targetPlayer, exists := players[notification.TargetLogin]
        // Verifica se o jogador existe, está online E tem uma conexão ativa neste servidor
        if exists && targetPlayer.Online && targetPlayer.Conn != nil {
            // Envia a mensagem recebida diretamente para a conexão TCP do jogador
            sendJSON(targetPlayer.Conn, notification.Message)
            logger.Printf(ColorGreen+"[API REST] Notificação enviada para %s (conectado localmente)"+ColorReset, notification.TargetLogin)
            w.WriteHeader(http.StatusOK) // Confirma que tentou (ou conseguiu) enviar
        } else {
             logger.Printf(ColorYellow+"[API REST] Jogador %s não conectado neste nó. Ignorando notificação."+ColorReset, notification.TargetLogin)
             w.WriteHeader(http.StatusNotFound) // Indica que o jogador não foi encontrado aqui
        }
        mu.Unlock()
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
	fsmRand = rand.New(rand.NewSource(12345))

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

	transport, err = raft.NewTCPTransport(*raftAddr, nil, 3, 10*time.Second, os.Stderr)
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

	activeTrades = make(map[string]*TradeOffer)

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
