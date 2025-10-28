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
	"sort"

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
	Jogador1  net.Conn // Estado volátil, não usado pela FSM
	Jogador2  net.Conn // Estado volátil, não usado pela FSM
	P1Login   string   // <-- ADICIONE: Login do P1 (estado RAFT)
	P2Login   string   // <-- ADICIONE: Login do P2 (estado RAFT)
	Status    string
	IsPrivate bool
	Game      *GameState // Estado do jogo (estado RAFT)
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
	parts := strings.SplitN(cmd, ":", 2)

	if len(parts) < 1 {
		return fsmApplyResponse{err: fmt.Errorf("comando FSM vazio")}
	}

	cmdType := parts[0]
	cmdPayload := ""
	if len(parts) > 1 {
		cmdPayload = parts[1]
	}

	switch cmdType {

	case "CADASTRAR_JOGADOR":
		var data protocolo.SignInRequest
		if err := json.Unmarshal([]byte(cmdPayload), &data); err != nil {
			return fsmApplyResponse{err: fmt.Errorf("FSM: falha ao decodificar cadastro")}
		}
		mu.Lock()
		defer mu.Unlock()
		if _, exists := players[data.Login]; exists {
			return fsmApplyResponse{err: fmt.Errorf("Login já existe.")}
		}
		players[data.Login] = &User{
			Login:  data.Login,
			Senha:  data.Senha,
			Moedas: 100,
		}
		logger.Printf("[FSM] Jogador %s cadastrado com sucesso.", data.Login)
		return fsmApplyResponse{response: "OK"}

	case "COMPRAR_PACOTE":
        playerLogin := cmdPayload
        mu.Lock()
        player, ok := players[playerLogin]
        mu.Unlock() // Desbloqueia logo após ler o player
        if !ok {
            logger.Printf("[FSM] Erro: jogador %s não encontrado para compra.", playerLogin)
            return fsmApplyResponse{err: fmt.Errorf("jogador não encontrado")}
        }

        stockMu.Lock() // Bloqueia apenas o estoque agora
        defer stockMu.Unlock()

        if player.Moedas < 20 {
            return fsmApplyResponse{response: protocolo.CompraResponse{Status: "NO_BALANCE"}}
        }

        var availablePacketIDs []string
        for id, packet := range packetStock {
            if !packet.Opened {
                availablePacketIDs = append(availablePacketIDs, id)
            }
        }

        if len(availablePacketIDs) == 0 {
            return fsmApplyResponse{response: protocolo.CompraResponse{Status: "EMPTY_STORAGE"}}
        }

        sort.Strings(availablePacketIDs)
        packetIDToGive := availablePacketIDs[0]
        packetToGive := packetStock[packetIDToGive]

        // Atualiza estado do jogador (precisa de Lock de novo)
        mu.Lock()
        player.Moedas -= 20
        packetToGive.Opened = true
        newInstrument := packetToGive.Instrument
        player.Inventario.Instrumentos = append(player.Inventario.Instrumentos, newInstrument)
        // Mantém cópia do inventário para resposta antes de desbloquear
        finalInventory := player.Inventario
        mu.Unlock()

        // Reabastecimento comentado
        /*
        newPkt := generatePacket(fsmRand, packetToGive.Rarity)
        if newPkt != nil {
            packetStock[newPkt.ID] = newPkt
        }
        */

        resp := protocolo.CompraResponse{
            Status:          "COMPRA_APROVADA",
            NovoInstrumento: &newInstrument,
            Inventario:      finalInventory, // Usa a cópia
        }
        return fsmApplyResponse{response: resp}


	case "PROPOSE_TRADE":
		var req struct {
			Player1Login string
			Player2Login string
			CardPlayer1  protocolo.Instrumento
		}
		if err := json.Unmarshal([]byte(cmdPayload), &req); err != nil {
			return fsmApplyResponse{err: fmt.Errorf("FSM: falha ao decodificar troca")}
		}

		mu.Lock()     // Bloqueia players
		tradeMu.Lock() // Bloqueia trades
		defer mu.Unlock()
		defer tradeMu.Unlock()

		if req.Player1Login == req.Player2Login {
			return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "Você não pode trocar cartas consigo mesmo."}}
		}
		player1, ok1 := players[req.Player1Login]
		player2, ok2 := players[req.Player2Login]
		if !ok1 || !ok2 {
			return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "Jogador não encontrado."}}
		}

		cardIndexP1 := -1
		for i, card := range player1.Inventario.Instrumentos {
			if card.ID == req.CardPlayer1.ID {
				cardIndexP1 = i
				break
			}
		}
		if cardIndexP1 == -1 {
			return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "Você não possui mais esta carta."}}
		}

		reverseTradeKey := req.Player2Login + ":" + req.Player1Login
		reverseOffer, exists := activeTrades[reverseTradeKey]

		if exists {
			cardP2 := reverseOffer.CardPlayer1
			cardIndexP2 := -1
			for i, card := range player2.Inventario.Instrumentos {
				if card.ID == cardP2.ID {
					cardIndexP2 = i
					break
				}
			}
			if cardIndexP2 == -1 {
				delete(activeTrades, reverseTradeKey)
				return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "O outro jogador não possui mais a carta oferecida. A oferta dele foi cancelada."}}
			}

			cardP1 := player1.Inventario.Instrumentos[cardIndexP1]
			player1.Inventario.Instrumentos = append(player1.Inventario.Instrumentos[:cardIndexP1], player1.Inventario.Instrumentos[cardIndexP1+1:]...)
			player2.Inventario.Instrumentos = append(player2.Inventario.Instrumentos[:cardIndexP2], player2.Inventario.Instrumentos[cardIndexP2+1:]...)
			player1.Inventario.Instrumentos = append(player1.Inventario.Instrumentos, cardP2)
			player2.Inventario.Instrumentos = append(player2.Inventario.Instrumentos, cardP1)
			delete(activeTrades, reverseTradeKey)

			respP1 := protocolo.TradeResponse{
				Status:           "TRADE_COMPLETED",
				Message:          fmt.Sprintf("Troca com %s concluída! Você recebeu: %s", player2.Login, cardP2.Name),
				Inventario:       player1.Inventario,
				ReceivedCard:     &cardP2,
				OtherPlayerLogin: player2.Login,
			}
			respP2 := protocolo.TradeResponse{
				Status:           "TRADE_COMPLETED",
				Message:          fmt.Sprintf("Troca com %s concluída! Você recebeu: %s", player1.Login, cardP1.Name),
				Inventario:       player2.Inventario,
				ReceivedCard:     &cardP1,
				OtherPlayerLogin: player1.Login,
			}
			logger.Printf("[FSM] Troca concluída entre %s e %s.", player1.Login, player2.Login)
			return fsmApplyResponse{response: FsmTradeCompletionResult{
				P1Login:       player1.Login,
				P2Login:       player2.Login,
				ResponseForP1: &respP1,
				ResponseForP2: &respP2,
			}}

		} else {
			tradeKey := req.Player1Login + ":" + req.Player2Login
			if _, exists := activeTrades[tradeKey]; exists {
				return fsmApplyResponse{response: protocolo.TradeResponse{Status: "ERRO", Message: "Você já tem uma oferta pendente para este jogador."}}
			}
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

	// --- NOVOS CASES PARA O JOGO ---

	case "CREATE_ROOM_RAFT":
		var data struct {
			RoomID       string `json:"room_id"`
			Player1Login string `json:"player1_login"`
			IsPrivate    bool   `json:"is_private"`
		}
		if err := json.Unmarshal([]byte(cmdPayload), &data); err != nil {
			return fsmApplyResponse{err: fmt.Errorf("FSM: falha ao decodificar create_room")}
		}
		mu.Lock()
		defer mu.Unlock()
		player1, ok := players[data.Player1Login]
		if !ok {
			logger.Printf(ColorYellow+"[FSM] Aviso: Jogador %s não encontrado para criar sala."+ColorReset, data.Player1Login)
			return fsmApplyResponse{response: "PlayerNotFound"}
		}
		if _, inRoom := playersInRoom[player1.Login]; inRoom {
			logger.Printf(ColorYellow+"[FSM] Aviso: Jogador %s já está em uma sala."+ColorReset, data.Player1Login)
			return fsmApplyResponse{response: "PlayerInRoom"}
		}
		novaSala := &Sala{
			ID:        data.RoomID,
			P1Login:   data.Player1Login, // Salva P1Login
			Status:    "Waiting_Player",
			IsPrivate: data.IsPrivate,
		}
		salas[data.RoomID] = novaSala
		playersInRoom[data.Player1Login] = novaSala
		if !data.IsPrivate {
			salasEmEspera = append(salasEmEspera, novaSala)
		}
		logger.Printf("[FSM] Sala %s criada para %s (Privada: %t).", data.RoomID, data.Player1Login, data.IsPrivate)
		return fsmApplyResponse{response: "RoomCreated"}

	case "JOIN_ROOM_RAFT":
		var data struct {
			RoomID       string `json:"room_id"`
			Player2Login string `json:"player2_login"`
		}
		if err := json.Unmarshal([]byte(cmdPayload), &data); err != nil {
			return fsmApplyResponse{err: fmt.Errorf("FSM: falha ao decodificar join_room")}
		}
		mu.Lock()
		defer mu.Unlock()
		_, ok := players[data.Player2Login] // Renomeado para _
		if !ok {
			logger.Printf(ColorYellow+"[FSM] Aviso: Jogador %s não encontrado para entrar na sala."+ColorReset, data.Player2Login)
			return fsmApplyResponse{response: "PlayerNotFound"}
		}
		if _, inRoom := playersInRoom[data.Player2Login]; inRoom {
			logger.Printf(ColorYellow+"[FSM] Aviso: Jogador %s já está em uma sala."+ColorReset, data.Player2Login)
			return fsmApplyResponse{response: "PlayerInRoom"}
		}
		sala, exists := salas[data.RoomID]
		if !exists {
			logger.Printf(ColorYellow+"[FSM] Aviso: Sala %s não encontrada para join."+ColorReset, data.RoomID)
			return fsmApplyResponse{response: "RoomNotFound"}
		}
		if sala.Status != "Waiting_Player" {
			logger.Printf(ColorYellow+"[FSM] Aviso: Sala %s não está esperando jogadores."+ColorReset, data.RoomID)
			return fsmApplyResponse{response: "RoomNotWaiting"}
		}
		sala.Status = "Em_Jogo"
		sala.P2Login = data.Player2Login // Salva P2Login
		playersInRoom[data.Player2Login] = sala
		if !sala.IsPrivate {
			for i, s := range salasEmEspera {
				if s.ID == sala.ID {
					salasEmEspera = append(salasEmEspera[:i], salasEmEspera[i+1:]...)
					break
				}
			}
		}
		sala.Game = &GameState{
			CurrentTurn:  1,
			PlayedNotes:  []string{},
			Player1Score: 0,
			Player2Score: 0,
		}
		logger.Printf("[FSM] Jogador %s entrou na sala %s. Jogo iniciado.", data.Player2Login, data.RoomID)
		return fsmApplyResponse{response: map[string]string{
			"status":  "GameStarted",
			"p1Login": sala.P1Login, // Usa o P1Login salvo
			"p2Login": data.Player2Login,
		}}

	case "PLAY_NOTE_RAFT":
		var data struct {
			PlayerLogin string `json:"player_login"`
			Note        string `json:"note"`
		}
		if err := json.Unmarshal([]byte(cmdPayload), &data); err != nil {
			return fsmApplyResponse{err: fmt.Errorf("FSM: falha ao decodificar play_note")}
		}
		mu.Lock()
		defer mu.Unlock()
		_, ok := players[data.PlayerLogin] // Renomeado para _
		if !ok {
			return fsmApplyResponse{err: fmt.Errorf("jogador não encontrado")}
		}
		sala, inRoom := playersInRoom[data.PlayerLogin]
		if !inRoom || sala.Game == nil || sala.Status != "Em_Jogo" {
			logger.Printf(ColorYellow+"[FSM] Aviso: Jogador %s tentou jogar nota fora de jogo ativo."+ColorReset, data.PlayerLogin)
			return fsmApplyResponse{response: "NotInGame"}
		}
		playerNum := 0
		if data.PlayerLogin == sala.P1Login { // Usa P1Login da sala
			playerNum = 1
		} else {
			playerNum = 2
		}
		if sala.Game.CurrentTurn != playerNum {
			logger.Printf(ColorYellow+"[FSM] Aviso: Não é a vez de %s jogar."+ColorReset, data.PlayerLogin)
			return fsmApplyResponse{response: "NotYourTurn"}
		}
		validNotes := "ABCDEFG"
		note := strings.ToUpper(data.Note)
		if len(note) != 1 || !strings.Contains(validNotes, note) {
			logger.Printf(ColorYellow+"[FSM] Aviso: Nota inválida '%s' de %s."+ColorReset, data.Note, data.PlayerLogin)
			return fsmApplyResponse{response: "InvalidNote"}
		}
		sala.Game.PlayedNotes = append(sala.Game.PlayedNotes, note)
		p1 := players[sala.P1Login] // Pega P1 usando P1Login da sala
		p2 := players[sala.P2Login] // Pega P2 usando P2Login da sala
		attackName, attackerLogin := checkAttackCompletionFSM(sala, p1, p2)
		sequenceStr := strings.Join(sala.Game.PlayedNotes, "-")
		roundResult := protocolo.RoundResultMessage{
			PlayedNote:      note,
			PlayerName:      data.PlayerLogin,
			CurrentSequence: sequenceStr,
			AttackTriggered: attackerLogin != "",
		}
		gameEnded := false
		winner := ""
		if attackerLogin != "" {
			roundResult.AttackName = attackName
			roundResult.AttackerName = attackerLogin
			attackerUser := players[attackerLogin]
			sala.Game.LastAttacker = attackerUser
			if attackerLogin == p1.Login {
				sala.Game.Player1Score++
			} else {
				sala.Game.Player2Score++
			}
			sala.Game.PlayedNotes = []string{}
			if sala.Game.Player1Score >= 2 || sala.Game.Player2Score >= 2 {
				gameEnded = true
				winner = determineWinnerAndUpdateCoinsFSM(sala, p1, p2)
			} else {
				if attackerLogin == p1.Login {
					sala.Game.CurrentTurn = 1
				} else {
					sala.Game.CurrentTurn = 2
				}
			}
		} else {
			if sala.Game.CurrentTurn == 1 {
				sala.Game.CurrentTurn = 2
			} else {
				sala.Game.CurrentTurn = 1
			}
		}
		fsmResult := map[string]interface{}{
			"status":      "NotePlayed",
			"roundResult": roundResult,
			"nextTurn":    sala.Game.CurrentTurn,
			"gameEnded":   gameEnded,
			"p1Login":     p1.Login,
			"p2Login":     p2.Login,
		}
		if gameEnded {
			fsmResult["status"] = "GameOver"
			fsmResult["winner"] = winner
		}
		logger.Printf("[FSM] Nota %s jogada por %s na sala %s.", note, data.PlayerLogin, sala.ID)
		return fsmApplyResponse{response: fsmResult}

	case "SELECT_INSTRUMENT_RAFT":
		var data struct {
			PlayerLogin   string `json:"player_login"`
			InstrumentoID string `json:"instrumento_id"` // <-- Mudei para ID (string)
		}
		if err := json.Unmarshal([]byte(cmdPayload), &data); err != nil {
			return fsmApplyResponse{err: fmt.Errorf("FSM: falha ao decodificar select_instrument")}
		}
		mu.Lock()
		defer mu.Unlock()

		player, ok := players[data.PlayerLogin]
		if !ok {
			logger.Printf(ColorYellow+"[FSM] Aviso: Jogador %s não encontrado para selecionar instrumento."+ColorReset, data.PlayerLogin)
			return fsmApplyResponse{response: "PlayerNotFound"}
		}

		// Encontra o instrumento no inventário do jogador PELO ID
		var foundInstrument *protocolo.Instrumento
		for _, inst := range player.Inventario.Instrumentos {
			if inst.ID == data.InstrumentoID {
				// IMPORTANTE: Precisamos copiar o instrumento para
				// player.SelectedInstrument para evitar problemas de ponteiro
				// com o slice do inventário.
				tempInst := inst 
				foundInstrument = &tempInst
				break
			}
		}

		if foundInstrument == nil {
			logger.Printf(ColorYellow+"[FSM] Aviso: Jogador %s tentou selecionar instrumento (%s) que não possui."+ColorReset, data.PlayerLogin, data.InstrumentoID)
			return fsmApplyResponse{response: "InstrumentNotFound"}
		}

		// Atualiza o estado REPLICADO
		player.SelectedInstrument = foundInstrument

		logger.Printf("[FSM] Jogador %s selecionou o instrumento %s.", data.PlayerLogin, foundInstrument.Name)
		return fsmApplyResponse{response: "Selected:" + foundInstrument.Name}

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

// func loadPlayerData() {
// 	mu.Lock()
// 	defer mu.Unlock()
// 	data, err := os.ReadFile(playerDataFile)
// 	if err != nil {
// 		if os.IsNotExist(err) {
// 			logger.Printf("Arquivo de jogadores (%s) não encontrado. Um novo será criado.", playerDataFile)
// 			players = make(map[string]*User)
// 		}
// 		return
// 	}
// 	json.Unmarshal(data, &players)
// 	logger.Printf("%d jogadores carregados.", len(players))
// }

// func savePlayerData() {
// 	logger.Println("\nSalvando dados dos jogadores...")
// 	mu.Lock()
// 	defer mu.Unlock()
// 	for _, player := range players {
// 		player.Online = false
// 		player.Conn = nil
// 	}
// 	data, _ := json.MarshalIndent(players, "", "  ")
// 	os.WriteFile(playerDataFile, data, 0644)
// 	logger.Printf("Dados de %d jogadores salvos.", len(players))
// }

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

// OUTRAS FUNCOES---
func findPlayerLoginBySala(sala *Sala, wantPlayer1 bool) string {
    // Implementando a MELHOR SOLUÇÃO: usa P1Login e P2Login da struct Sala
    if wantPlayer1 {
        return sala.P1Login
    }
    return sala.P2Login
}


// Versão FSM-safe de checkAttackCompletion. Retorna login do atacante.
// PRECISA ser chamada com 'mu' bloqueado.
func checkAttackCompletionFSM(sala *Sala, p1 *User, p2 *User) (string, string) {
	sequence := strings.Join(sala.Game.PlayedNotes, "")
	if p1.SelectedInstrument != nil {
		for _, attack := range p1.SelectedInstrument.Attacks {
			attackSeq := strings.Join(attack.Sequence, "")
			if strings.HasSuffix(sequence, attackSeq) {
				return attack.Name, p1.Login
			}
		}
	}
	if p2.SelectedInstrument != nil {
		for _, attack := range p2.SelectedInstrument.Attacks {
			attackSeq := strings.Join(attack.Sequence, "")
			if strings.HasSuffix(sequence, attackSeq) {
				return attack.Name, p2.Login
			}
		}
	}
	return "", ""
}

// Função para determinar o vencedor e ATUALIZAR MOEDAS DENTRO DA FSM.
// PRECISA ser chamada com 'mu' bloqueado.
func determineWinnerAndUpdateCoinsFSM(sala *Sala, p1 *User, p2 *User) string {
	game := sala.Game
	p1CoinsEarned := game.Player1Score * 5
	p2CoinsEarned := game.Player2Score * 5
	winner := "EMPATE"

	if game.Player1Score > game.Player2Score {
		winner = p1.Login
		p1CoinsEarned += 20
	} else if game.Player2Score > game.Player1Score {
		winner = p2.Login
		p2CoinsEarned += 20
	}

	p1.Moedas += p1CoinsEarned
	p2.Moedas += p2CoinsEarned

	logger.Printf("[FSM] Fim de jogo na sala %s. Vencedor: %s. P1 ganhou %d, P2 ganhou %d.", sala.ID, winner, p1CoinsEarned, p2CoinsEarned)
	sala.Status = "Finished" // Marca a sala como terminada na FSM
	return winner
}

// Adicione esta função auxiliar para encaminhamento REST genérico
func forwardRequestToLeader(endpoint string, payload map[string]interface{}, originalConn net.Conn) {
	leaderRaftAddr := string(raftNode.Leader())
	if leaderRaftAddr == "" {
		sendScreenMsg(originalConn, "Erro interno: líder indisponível.")
		return
	}
	// Converte endereço RAFT (ex: servidor1:12000) para HTTP (ex: servidor1:8090)
	leaderHttpAddr := strings.Replace(leaderRaftAddr, ":12000", ":8090", 1)
	logger.Printf(ColorYellow+"Encaminhando requisição para %s no líder %s"+ColorReset, endpoint, leaderHttpAddr)

	postBody, err := json.Marshal(payload)
    if err != nil {
         logger.Printf(ColorRed+"Erro ao serializar payload para encaminhamento (%s): %v"+ColorReset, endpoint, err)
         sendScreenMsg(originalConn, "Erro interno ao preparar requisição.")
         return
    }

	reqURL := fmt.Sprintf("http://%s%s", leaderHttpAddr, endpoint)

	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(reqURL, "application/json", bytes.NewBuffer(postBody))

	if err != nil {
		logger.Printf(ColorRed+"Erro ao encaminhar para o líder (%s): %v"+ColorReset, endpoint, err)
		sendScreenMsg(originalConn, "Erro ao contatar o servidor líder.")
		return
	}
	defer resp.Body.Close()

	// Lê a resposta completa do líder
	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		 logger.Printf(ColorRed+"Erro ao ler resposta do líder (%s): %v"+ColorReset, endpoint, readErr)
		 sendScreenMsg(originalConn, "Erro ao ler resposta do líder.")
		 return
	}

	// Tenta decodificar a resposta do líder como protocolo.Message e reenvia
	var leaderMsg protocolo.Message
	if err := json.Unmarshal(bodyBytes, &leaderMsg); err == nil {
		 // Reenvia a mensagem completa do líder para o cliente original
		 sendJSON(originalConn, leaderMsg)
	} else {
		 // Se não for uma protocolo.Message, talvez seja um erro HTTP direto?
		 logger.Printf(ColorYellow+"Resposta não padrão do líder (%s): Status %d, Corpo: %s"+ColorReset, endpoint, resp.StatusCode, string(bodyBytes))
		 if resp.StatusCode != http.StatusOK {
			   // Tenta extrair uma mensagem de erro se possível, senão manda genérico
			   var errorResp struct { Message string } // Tenta formato erro comum
			   if json.Unmarshal(bodyBytes, &errorResp) == nil && errorResp.Message != "" {
				    sendScreenMsg(originalConn, fmt.Sprintf("Erro do líder: %s", errorResp.Message))
			   } else {
				    sendScreenMsg(originalConn, fmt.Sprintf("Erro %d do servidor líder.", resp.StatusCode))
			   }
		 } else {
			  // Se foi OK mas não Message, algo está estranho. Envia como debug.
			  sendScreenMsg(originalConn, fmt.Sprintf("[DEBUG] Resposta inesperada do líder: %s", string(bodyBytes)))
		 }
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


func randomGenerateDeterministic(r *rand.Rand, length int) string {
    const charset = "ACDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
    b := make([]byte, length)
    for i := range b {
        b[i] = charset[r.Intn(len(charset))] // Usa o 'r' (o rand determinístico)
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
	player := findPlayerByConn(conn)
	if player == nil { sendScreenMsg(conn, "Erro: jogador não encontrado."); return }
	if player.SelectedInstrument == nil { sendScreenMsg(conn, "Selecione um instrumento primeiro!"); return }
    mu.Lock() // Protege playersInRoom
	_, inRoom := playersInRoom[player.Login]
    mu.Unlock()
	if inRoom { sendScreenMsg(conn, "Você já está em uma sala."); return }

	// --- Lógica Líder/Seguidor ---
	if raftNode.State() != raft.Leader {
		// Seguidor: Encaminha para o líder
		payload := map[string]interface{}{
			"player_login": player.Login,
			"mode":         mode,
			"room_code":    roomCode,
		}
		forwardRequestToLeader("/request-find-room", payload, conn)
	} else {
		// Líder: Processa a busca/criação/join via RAFT
		if mode == "PUBLIC" {
			mu.Lock() // Acesso rápido à lista de espera
			waitingRoomExists := len(salasEmEspera) > 0
			var targetRoomID string
			if waitingRoomExists {
				targetRoomID = salasEmEspera[0].ID // Pega o ID da primeira sala esperando
			}
			mu.Unlock()

			if waitingRoomExists {
				// Tenta entrar na sala pública existente via RAFT
				logger.Printf(ColorGreen+"Líder: Jogador %s tentando entrar na sala pública %s"+ColorReset, player.Login, targetRoomID)
				cmdPayload, _ := json.Marshal(map[string]interface{}{
					"room_id":      targetRoomID,
					"player2_login": player.Login,
				})
				cmd := []byte("JOIN_ROOM_RAFT:" + string(cmdPayload))
				applyFuture := raftNode.Apply(cmd, 500*time.Millisecond)
				handleJoinRoomResult(applyFuture, conn, player, targetRoomID) // Função auxiliar para tratar resultado
			} else {
				// Cria uma nova sala pública via RAFT
				logger.Printf(ColorGreen+"Líder: Jogador %s criando nova sala pública"+ColorReset, player.Login)
				roomID := randomGenerate(6)
				cmdPayload, _ := json.Marshal(map[string]interface{}{
					"room_id":      roomID,
					"player1_login": player.Login,
					"is_private":   false,
				})
				cmd := []byte("CREATE_ROOM_RAFT:" + string(cmdPayload))
				applyFuture := raftNode.Apply(cmd, 500*time.Millisecond)

				// Tratar resultado de criação
				if err := applyFuture.Error(); err != nil { logger.Printf(ColorRed+"Erro RAFT ao criar sala pública para %s: %v"+ColorReset, player.Login, err); sendScreenMsg(conn, "Erro RAFT ao criar sala."); return }
				fsmResp := applyFuture.Response().(fsmApplyResponse)
				if fsmResp.err != nil || fsmResp.response != "RoomCreated" { logger.Printf(ColorYellow+"Falha FSM ao criar sala pública para %s: %v %v"+ColorReset, player.Login, fsmResp.err, fsmResp.response); sendScreenMsg(conn, fmt.Sprintf("Erro ao criar sala: %v", fsmResp.response)); return }

				// Associa Conn após FSM confirmar
				mu.Lock()
				sala, ok := salas[roomID]
				if ok { sala.Jogador1 = conn } else { logger.Printf(ColorRed+"Erro CRÍTICO: Sala pública %s criada pela FSM mas não encontrada!"+ColorReset, roomID) }
				mu.Unlock()
				sendScreenMsg(conn, "Aguardando outro jogador na sala pública...")
			}
		} else if roomCode != "" {
			// Tenta entrar na sala privada via RAFT
			logger.Printf(ColorGreen+"Líder: Jogador %s tentando entrar na sala privada %s"+ColorReset, player.Login, roomCode)
			cmdPayload, _ := json.Marshal(map[string]interface{}{
				"room_id":      roomCode,
				"player2_login": player.Login,
			})
			cmd := []byte("JOIN_ROOM_RAFT:" + string(cmdPayload))
			applyFuture := raftNode.Apply(cmd, 500*time.Millisecond)
			handleJoinRoomResult(applyFuture, conn, player, roomCode) // Mesma função auxiliar
		} else {
            // Caso inválido (nem público, nem código)
             sendScreenMsg(conn, "Requisição de sala inválida.")
        }
	}
}

// Função auxiliar para tratar o resultado do Apply de JOIN_ROOM_RAFT (chamada pelo LÍDER)
func handleJoinRoomResult(future raft.ApplyFuture, conn net.Conn, player *User, roomID string) {
	if err := future.Error(); err != nil {
		logger.Printf(ColorRed+"Erro RAFT ao tentar JOIN_ROOM_RAFT para %s na sala %s: %v"+ColorReset, player.Login, roomID, err)
		sendScreenMsg(conn, "Erro interno ao entrar na sala (RAFT).")
		return
	}
	fsmResp := future.Response().(fsmApplyResponse)
	if fsmResp.err != nil {
        logger.Printf(ColorRed+"Erro FSM ao tentar JOIN_ROOM_RAFT para %s na sala %s: %v"+ColorReset, player.Login, roomID, fsmResp.err)
		sendScreenMsg(conn, fmt.Sprintf("Erro FSM ao entrar na sala: %v", fsmResp.err))
		return
	}

	// Verifica a resposta da FSM
	switch result := fsmResp.response.(type) {
	case string: // Erro de negócio (ex: "RoomNotFound", "PlayerInRoom", "RoomNotWaiting")
         logger.Printf(ColorYellow+"Falha FSM ao tentar JOIN_ROOM_RAFT para %s na sala %s: %s"+ColorReset, player.Login, roomID, result)
		 sendScreenMsg(conn, fmt.Sprintf("Não foi possível entrar na sala: %s", result))
	case map[string]string: // Sucesso - Jogo Iniciado
		if status, ok := result["status"]; ok && status == "GameStarted" {
			p1Login := result["p1Login"]
			p2Login := result["p2Login"]
			logger.Printf(ColorGreen+"JOIN_ROOM_RAFT sucedido para %s na sala %s (P1: %s, P2: %s)."+ColorReset, player.Login, roomID, p1Login, p2Login)

			// Líder associa as conexões (estado volátil) APÓS FSM confirmar
			mu.Lock()
			sala, exists := salas[roomID]
            p1, p1ok := players[p1Login] // Busca P1 para pegar Conn
            p2, p2ok := players[p2Login] // Busca P2 para pegar Conn

			if exists {
                 // Associa/Confirma as conexões reais aos slots da sala
				 if sala.P1Login == p1Login && p1ok && p1.Online { sala.Jogador1 = p1.Conn }
                 if sala.P2Login == p2Login && p2ok && p2.Online { sala.Jogador2 = p2.Conn }

				 // Encontra a conexão do OUTRO jogador (pode ser nil se ele estiver em outro nó)
				 otherPlayerConn := net.Conn(nil)
				 if player.Login == p1Login && p2ok && p2.Online { // Se eu sou P1, notifica P2 se ele estiver local
					 otherPlayerConn = p2.Conn
				 } else if player.Login == p2Login && p1ok && p1.Online { // Se eu sou P2, notifica P1 se ele estiver local
                     otherPlayerConn = p1.Conn
                 }

				// Envia PAREADO para o jogador ATUAL (que fez a requisição)
                // Usamos o login da FSM para garantir consistência
				sendPairing(conn, player.Login, roomID)

                // Envia PAREADO para o OUTRO jogador SE ele estiver conectado a ESTE líder
                if otherPlayerConn != nil {
                     otherLogin := ""
                     if player.Login == p1Login { otherLogin = p2Login } else { otherLogin = p1Login }
                     sendPairing(otherPlayerConn, otherLogin, roomID)
                } else {
                     // Se o outro jogador não está aqui, o broadcast fará o trabalho
                     otherLogin := ""
                      if player.Login == p1Login { otherLogin = p2Login } else { otherLogin = p1Login }
                      otherPairingMsg := protocolo.Message{Type: "PAREADO", Data: protocolo.PairingMessage{Status: "PAREADO", SalaID: roomID, PlayerLogin: otherLogin}}
                      broadcastNotificationToPlayer(otherLogin, otherPairingMsg) // Pede aos outros nós para enviarem PAREADO
                }

				// Inicia o jogo e envia MQTTs (somente o líder faz isso)
				// É importante iniciar DEPOIS de enviar PAREADO
				go startGameAndNotify(sala)

			} else {
                 logger.Printf(ColorRed+"Erro CRÍTICO: Sala %s iniciada pela FSM mas não encontrada localmente!"+ColorReset, roomID)
            }
			mu.Unlock()
		} else {
            logger.Printf(ColorRed+"Erro: Resposta inesperada da FSM para JOIN_ROOM_RAFT: %v"+ColorReset, result)
			sendScreenMsg(conn, "Resposta inesperada do servidor ao entrar na sala.")
		}
	default:
         logger.Printf(ColorRed+"Erro: Resposta desconhecida da FSM para JOIN_ROOM_RAFT: %v"+ColorReset, fsmResp.response)
		 sendScreenMsg(conn, "Resposta desconhecida do servidor ao entrar na sala.")
	}
}

// handleJoinRoomResultREST (Nova função para a API REST)
// Trata o resultado do Apply de JOIN_ROOM_RAFT vindo de um seguidor (via HTTP)
func handleJoinRoomResultREST(future raft.ApplyFuture, w http.ResponseWriter, playerLogin string, roomID string) {
	if err := future.Error(); err != nil {
		logger.Printf(ColorRed+"[API REST] Erro RAFT ao tentar JOIN_ROOM_RAFT para %s na sala %s: %v"+ColorReset, playerLogin, roomID, err)
		http.Error(w, "Erro interno ao entrar na sala (RAFT).", http.StatusInternalServerError)
		return
	}
	fsmResp := future.Response().(fsmApplyResponse)
	if fsmResp.err != nil {
		logger.Printf(ColorRed+"[API REST] Erro FSM ao tentar JOIN_ROOM_RAFT para %s na sala %s: %v"+ColorReset, playerLogin, roomID, fsmResp.err)
		http.Error(w, fmt.Sprintf("Erro FSM ao entrar na sala: %v", fsmResp.err), http.StatusInternalServerError)
		return
	}

	// Prepara a resposta HTTP
	w.Header().Set("Content-Type", "application/json")

	switch result := fsmResp.response.(type) {
	case string: // Erro de negócio (ex: "RoomNotFound", "PlayerInRoom", "RoomNotWaiting")
		logger.Printf(ColorYellow+"[API REST] Falha FSM ao tentar JOIN_ROOM_RAFT para %s na sala %s: %s"+ColorReset, playerLogin, roomID, result)
		// Retorna um erro HTTP 400 (Bad Request) com a mensagem
		w.WriteHeader(http.StatusBadRequest)
		// Envia a mesma estrutura de mensagem que o cliente esperaria
		respMsg := protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: fmt.Sprintf("Não foi possível entrar na sala: %s", result)}}
		json.NewEncoder(w).Encode(respMsg)

	case map[string]string: // Sucesso - Jogo Iniciado
		if status, ok := result["status"]; ok && status == "GameStarted" {
			p1Login := result["p1Login"]
			p2Login := result["p2Login"]
			logger.Printf(ColorGreen+"[API REST] JOIN_ROOM_RAFT sucedido para %s na sala %s (P1: %s, P2: %s)."+ColorReset, playerLogin, roomID, p1Login, p2Login)

			// Lógica do Líder (quase idêntica a handleJoinRoomResult)
			mu.Lock()
			sala, exists := salas[roomID]
			p1, p1ok := players[p1Login] // Busca P1
			p2, p2ok := players[p2Login] // Busca P2

			if exists {
				// Encontra a conexão do OUTRO jogador (pode ser nil se ele estiver em outro nó)
				otherPlayerConn := net.Conn(nil)
				var otherLogin string

				if playerLogin == p1Login { // Se eu (requisitante HTTP) sou P1
					otherLogin = p2Login
					if p2ok && p2.Online {
						otherPlayerConn = p2.Conn
					}
				} else { // Se eu (requisitante HTTP) sou P2
					otherLogin = p1Login
					if p1ok && p1.Online {
						otherPlayerConn = p1.Conn
					}
				}

				// Envia PAREADO para o OUTRO jogador SE ele estiver conectado a ESTE líder
				if otherPlayerConn != nil {
					sendPairing(otherPlayerConn, otherLogin, roomID)
				} else {
					// Se o outro jogador não está aqui, o broadcast fará o trabalho
					otherPairingMsg := protocolo.Message{Type: "PAREADO", Data: protocolo.PairingMessage{Status: "PAREADO", SalaID: roomID, PlayerLogin: otherLogin}}
					broadcastNotificationToPlayer(otherLogin, otherPairingMsg)
				}

				// Inicia o jogo e envia MQTTs (somente o líder faz isso)
				go startGameAndNotify(sala)

			} else {
				logger.Printf(ColorRed+"[API REST] Erro CRÍTICO: Sala %s iniciada pela FSM mas não encontrada localmente!"+ColorReset, roomID)
			}
			mu.Unlock()

			// Responde PAREADO para o jogador ATUAL (que fez a requisição HTTP)
			w.WriteHeader(http.StatusOK)
			respMsg := protocolo.Message{Type: "PAREADO", Data: protocolo.PairingMessage{Status: "PAREADO", SalaID: roomID, PlayerLogin: playerLogin}}
			json.NewEncoder(w).Encode(respMsg)

		} else {
			logger.Printf(ColorRed+"[API REST] Erro: Resposta inesperada da FSM para JOIN_ROOM_RAFT: %v"+ColorReset, result)
			http.Error(w, "Resposta inesperada do servidor ao entrar na sala.", http.StatusInternalServerError)
		}
	default:
		logger.Printf(ColorRed+"[API REST] Erro: Resposta desconhecida da FSM para JOIN_ROOM_RAFT: %v"+ColorReset, fsmResp.response)
		http.Error(w, "Resposta desconhecida do servidor ao entrar na sala.", http.StatusInternalServerError)
	}
}

// Nova função para iniciar o jogo e enviar MQTTs (chamada pelo LÍDER)
func startGameAndNotify(sala *Sala) {
	if raftNode.State() != raft.Leader { return } // Apenas o líder notifica via MQTT

	// A FSM já inicializou sala.Game. Apenas lemos os dados necessários.
	mu.Lock() // Protege acesso a sala, players
	p1Login := sala.P1Login
	p2Login := sala.P2Login
    salaID := sala.ID
    currentTurn := 0 // Default
    if sala.Game != nil { // Checa se Game foi inicializado
        currentTurn = sala.Game.CurrentTurn
    }
	mu.Unlock()

    if p1Login == "" || p2Login == "" {
         logger.Printf(ColorRed+"Erro ao notificar início de jogo: Logins P1/P2 não encontrados na sala %s"+ColorReset, salaID)
         return
    }
    if currentTurn == 0 {
        logger.Printf(ColorRed+"Erro ao notificar início de jogo: Estado do jogo não inicializado na sala %s"+ColorReset, salaID)
        return
    }


	logger.Printf(ColorGreen+"Líder: Enviando notificações de início de jogo para sala %s (P1: %s, P2: %s)"+ColorReset, salaID, p1Login, p2Login)

	// Envia GAME_START via MQTT
	publishToPlayer(salaID, p1Login, protocolo.Message{Type: "GAME_START", Data: protocolo.GameStartMessage{Opponent: p2Login}})
	publishToPlayer(salaID, p2Login, protocolo.Message{Type: "GAME_START", Data: protocolo.GameStartMessage{Opponent: p1Login}})

	time.Sleep(1 * time.Second) // Pequena pausa

	// Envia primeiro TURN_UPDATE via MQTT
	publishToPlayer(salaID, p1Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: currentTurn == 1}})
	publishToPlayer(salaID, p2Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: currentTurn == 2}})
}

// Pequena Modificação em sendPairing para receber login e roomID
func sendPairing(conn net.Conn, playerLogin string, roomID string) {
    if conn == nil { return } // Checagem extra
    msg := protocolo.Message{Type: "PAREADO", Data: protocolo.PairingMessage{Status: "PAREADO", SalaID: roomID, PlayerLogin: playerLogin}}
    sendJSON(conn, msg)
}

func createRoom(conn net.Conn) {
	player := findPlayerByConn(conn)
	if player == nil {
		sendScreenMsg(conn, "Erro: jogador não encontrado.")
		return
	}
	if player.SelectedInstrument == nil {
		sendScreenMsg(conn, "Você precisa selecionar um instrumento para a batalha primeiro!")
		return
	}
    // Verifica se já está em sala (Leitura rápida antes de encaminhar)
    mu.Lock()
	_, inRoom := playersInRoom[player.Login]
    mu.Unlock()
	if inRoom {
		sendScreenMsg(conn, "Você já está em uma sala.")
		return
	}

	// --- Lógica Líder/Seguidor ---
	if raftNode.State() != raft.Leader {
		// Seguidor: Encaminha para o líder via REST
		forwardRequestToLeader("/request-create-room", map[string]interface{}{"player_login": player.Login}, conn)
	} else {
		// Líder: Gera ID e aplica no RAFT
		roomID := randomGenerate(6) // Líder gera o ID não determinístico globalmente
		cmdPayload, _ := json.Marshal(map[string]interface{}{
			"room_id":      roomID,
			"player1_login": player.Login,
			"is_private":   true, // createRoom é sempre privado
		})
		cmd := []byte("CREATE_ROOM_RAFT:" + string(cmdPayload))

		future := raftNode.Apply(cmd, 500*time.Millisecond)
		if err := future.Error(); err != nil {
			logger.Printf(ColorRed+"Erro RAFT ao criar sala para %s: %v"+ColorReset, player.Login, err)
			sendScreenMsg(conn, "Erro interno ao criar sala (RAFT).")
			return
		}
		// A FSM retorna "RoomCreated", "PlayerNotFound", ou "PlayerInRoom"
		fsmResp := future.Response().(fsmApplyResponse)
		if fsmResp.err != nil {
             logger.Printf(ColorRed+"Erro FSM ao criar sala para %s: %v"+ColorReset, player.Login, fsmResp.err)
			 sendScreenMsg(conn, fmt.Sprintf("Erro FSM ao criar sala: %v", fsmResp.err))
             return
        }
        if fsmResp.response != "RoomCreated" {
             logger.Printf(ColorYellow+"Falha FSM ao criar sala para %s: %v"+ColorReset, player.Login, fsmResp.response)
			 sendScreenMsg(conn, fmt.Sprintf("Não foi possível criar sala: %s", fsmResp.response)) // Ex: "Jogador já está em sala"
             return
		}

		// Sucesso: Associa Conn (estado volátil) APÓS FSM confirmar
		mu.Lock()
		sala, ok := salas[roomID]
		if ok {
            sala.Jogador1 = conn // Associa a conexão real
             // Adiciona P1Login aqui também, embora FSM já tenha feito, para consistência local imediata?
             // sala.P1Login = player.Login // Já feito na FSM
        } else {
             logger.Printf(ColorRed+"Erro CRÍTICO: Sala %s criada pela FSM mas não encontrada localmente!"+ColorReset, roomID)
        }
		mu.Unlock()
		sendScreenMsg(conn, "Sala privada criada. Código: "+roomID)
	}
}

// func sendPairing(conn net.Conn) {
// 	p := findPlayerByConn(conn)
// 	sala := playersInRoom[conn.RemoteAddr().String()]
// 	msg := protocolo.Message{Type: "PAREADO", Data: protocolo.PairingMessage{Status: "PAREADO", SalaID: sala.ID, PlayerLogin: p.Login}}
// 	sendJSON(conn, msg)
// }

// --- LÓGICA DO JOGO MUSICAL ---

// func startGame(sala *Sala) {
// 	p1 := findPlayerByConn(sala.Jogador1)
// 	p2 := findPlayerByConn(sala.Jogador2)
// 	if p1 == nil || p2 == nil {
// 		return
// 	}

// 	sala.Game = &GameState{
// 		CurrentTurn: 1,
// 	}

// 	publishToPlayer(sala.ID, p1.Login, protocolo.Message{Type: "GAME_START", Data: protocolo.GameStartMessage{Opponent: p2.Login}})
// 	publishToPlayer(sala.ID, p2.Login, protocolo.Message{Type: "GAME_START", Data: protocolo.GameStartMessage{Opponent: p1.Login}})

// 	time.Sleep(1 * time.Second)
// 	notifyTurn(sala)
// }

// func notifyTurn(sala *Sala) {
// 	p1 := findPlayerByConn(sala.Jogador1)
// 	p2 := findPlayerByConn(sala.Jogador2)
// 	if p1 == nil || p2 == nil {
// 		return
// 	}

// 	publishToPlayer(sala.ID, p1.Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: sala.Game.CurrentTurn == 1}})
// 	publishToPlayer(sala.ID, p2.Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: sala.Game.CurrentTurn == 2}})
// }

// Porteiro para PLAY_NOTE
func handlePlayNote(conn net.Conn, data interface{}) {
    var req protocolo.PlayNoteRequest
    if err := mapToStruct(data, &req); err != nil { // Pega a nota
         sendScreenMsg(conn, "Formato de nota inválido.")
         return
    }

    player := findPlayerByConn(conn)
    if player == nil { sendScreenMsg(conn, "Erro: jogador não encontrado."); return }

    // Validação rápida (está em jogo?) - A FSM fará a validação final
    mu.Lock() // Protege playersInRoom
    _, inRoom := playersInRoom[player.Login]
    mu.Unlock()
    if !inRoom {
        sendScreenMsg(conn, "Você não está em um jogo ativo.")
        return
    }

    // --- Lógica Líder/Seguidor ---
    if raftNode.State() != raft.Leader {
        // Seguidor: Encaminha para o líder
        payload := map[string]interface{}{
            "player_login": player.Login,
            "note":         req.Note,
        }
        forwardRequestToLeader("/request-play-note", payload, conn)
    } else {
        // Líder: Aplica no RAFT
        cmdPayload, _ := json.Marshal(map[string]interface{}{
            "player_login": player.Login,
            "note":         req.Note,
        })
        cmd := []byte("PLAY_NOTE_RAFT:" + string(cmdPayload))

        future := raftNode.Apply(cmd, 500*time.Millisecond)
        if err := future.Error(); err != nil {
            logger.Printf(ColorRed+"Erro RAFT ao jogar nota por %s: %v"+ColorReset, player.Login, err)
            sendScreenMsg(conn, "Erro interno ao jogar nota (RAFT).")
            return
        }
        fsmResp := future.Response().(fsmApplyResponse)
         if fsmResp.err != nil {
             logger.Printf(ColorRed+"Erro FSM ao jogar nota por %s: %v"+ColorReset, player.Login, fsmResp.err)
             sendScreenMsg(conn, fmt.Sprintf("Erro FSM ao jogar nota: %v", fsmResp.err))
             return
         }

        // Processa o resultado da FSM
        switch resultData := fsmResp.response.(type) {
        case string: // Erro de negócio (ex: "NotInGame", "NotYourTurn", "InvalidNote")
             logger.Printf(ColorYellow+"Falha FSM ao jogar nota por %s: %s"+ColorReset, player.Login, resultData)
             sendScreenMsg(conn, fmt.Sprintf("Não foi possível jogar: %s", resultData))
        case map[string]interface{}: // Sucesso (NotePlayed ou GameOver)
             // Líder envia atualizações MQTT
             p1Login := resultData["p1Login"].(string)
             p2Login := resultData["p2Login"].(string)
             salaID := ""
             mu.Lock() // Protege playersInRoom e salas
             sala, ok := playersInRoom[player.Login] // Pode ser P1 ou P2
             if ok && sala != nil { salaID = sala.ID }
             // Pega pontuações atuais para mensagens MQTT
             currentP1Score, currentP2Score := 0, 0
             if ok && sala != nil && sala.Game != nil {
                 currentP1Score = sala.Game.Player1Score
                 currentP2Score = sala.Game.Player2Score
             }
             mu.Unlock()

             if salaID == "" { logger.Printf(ColorRed+"Erro CRÍTICO: Sala não encontrada para %s ao processar nota."+ColorReset, player.Login); return }

             // Envia ROUND_RESULT via MQTT
             if roundResultData, ok := resultData["roundResult"]; ok {
                  var roundResult protocolo.RoundResultMessage
                  mapToStruct(roundResultData, &roundResult)

                  resultMsgP1 := roundResult
                  resultMsgP1.YourScore = currentP1Score // Usa pontuação lida
                  resultMsgP1.OpponentScore = currentP2Score
                  publishToPlayer(salaID, p1Login, protocolo.Message{Type: "ROUND_RESULT", Data: resultMsgP1})

                  resultMsgP2 := roundResult
                  resultMsgP2.YourScore = currentP2Score // Usa pontuação lida
                  resultMsgP2.OpponentScore = currentP1Score
                  publishToPlayer(salaID, p2Login, protocolo.Message{Type: "ROUND_RESULT", Data: resultMsgP2})
             }

             // Verifica se o jogo acabou
             if gameEnded, _ := resultData["gameEnded"].(bool); gameEnded {
                 winner := resultData["winner"].(string)
                 mu.Lock() // Protege players para recalcular moedas ganhas
                 finalP1Score, finalP2Score := 0, 0
                 // Busca sala novamente para garantir que ainda existe
                 sala, salaOk := salas[salaID]
                 if salaOk && sala != nil && sala.Game != nil {
                    finalP1Score = sala.Game.Player1Score
                    finalP2Score = sala.Game.Player2Score
                 }
                 mu.Unlock() // Desbloqueia antes de calcular

                 // Recalcula moedas ganhas baseado na pontuação final
                 p1CoinsEarned := finalP1Score*5 + (func() int { if winner == p1Login { return 20 }; return 0 }())
                 p2CoinsEarned := finalP2Score*5 + (func() int { if winner == p2Login { return 20 }; return 0 }())

                 // Envia GAME_OVER via MQTT
                  gameOverMsgP1 := protocolo.GameOverMessage{ Winner: winner, FinalScoreP1: finalP1Score, FinalScoreP2: finalP2Score, CoinsEarned: p1CoinsEarned }
                  publishToPlayer(salaID, p1Login, protocolo.Message{Type: "GAME_OVER", Data: gameOverMsgP1})
                  gameOverMsgP2 := protocolo.GameOverMessage{ Winner: winner, FinalScoreP1: finalP1Score, FinalScoreP2: finalP2Score, CoinsEarned: p2CoinsEarned }
                  publishToPlayer(salaID, p2Login, protocolo.Message{Type: "GAME_OVER", Data: gameOverMsgP2})

                  // Líder limpa o estado da sala (fora da FSM, após notificações)
                  mu.Lock()
                  delete(playersInRoom, p1Login)
                  delete(playersInRoom, p2Login)
                  delete(salas, salaID)
                  mu.Unlock()
                  logger.Printf(ColorGreen+"Jogo na sala %s finalizado pelo líder."+ColorReset, salaID)

             } else {
                  // Envia TURN_UPDATE via MQTT
                  // Certifique-se de que nextTurn é tratado como float64 vindo do JSON/map
                  // --- CORRIGIDO ---
				nextTurnPlayerInt, ok := resultData["nextTurn"].(int)
				if !ok {
					// Se não for int, TENTA float64 (para o caso de vir de um JSON)
					if nextTurnPlayerFloat, okFloat := resultData["nextTurn"].(float64); okFloat {
						nextTurnPlayerInt = int(nextTurnPlayerFloat)
					} else {
						logger.Printf(ColorRed+"Erro CRÍTICO: 'nextTurn' não é nem int nem float64 no resultado da FSM. Tipo: %T"+ColorReset, resultData["nextTurn"])
						return
					}
				}
				nextTurnPlayer := nextTurnPlayerInt
                publishToPlayer(salaID, p1Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: nextTurnPlayer == 1}})
                publishToPlayer(salaID, p2Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: nextTurnPlayer == 2}})
             }
        default:
            logger.Printf(ColorRed+"Erro: Resposta desconhecida da FSM para PLAY_NOTE_RAFT: %v"+ColorReset, fsmResp.response)
            sendScreenMsg(conn, "Erro inesperado ao processar nota.")
        }
    }
}

// func checkAttackCompletion(sala *Sala) (string, *User) {
// 	p1 := findPlayerByConn(sala.Jogador1)
// 	p2 := findPlayerByConn(sala.Jogador2)
// 	sequence := strings.Join(sala.Game.PlayedNotes, "")

// 	if p1.SelectedInstrument != nil {
// 		for _, attack := range p1.SelectedInstrument.Attacks {
// 			attackSeq := strings.Join(attack.Sequence, "")
// 			if strings.HasSuffix(sequence, attackSeq) {
// 				return attack.Name, p1
// 			}
// 		}
// 	}
// 	if p2.SelectedInstrument != nil {
// 		for _, attack := range p2.SelectedInstrument.Attacks {
// 			attackSeq := strings.Join(attack.Sequence, "")
// 			if strings.HasSuffix(sequence, attackSeq) {
// 				return attack.Name, p2
// 			}
// 		}
// 	}
// 	return "", nil
// }

// func endGame(sala *Sala) {
// 	game := sala.Game
// 	p1 := findPlayerByConn(sala.Jogador1)
// 	p2 := findPlayerByConn(sala.Jogador2)

// 	p1.Moedas += game.Player1Score * 5
// 	p2.Moedas += game.Player2Score * 5

// 	var winner string
// 	if game.Player1Score > game.Player2Score {
// 		winner = p1.Login
// 		p1.Moedas += 20
// 	} else if game.Player2Score > game.Player1Score {
// 		winner = p2.Login
// 		p2.Moedas += 20
// 	} else {
// 		winner = "EMPATE"
// 	}

// 	gameOverMsgP1 := protocolo.GameOverMessage{
// 		Winner:       winner,
// 		FinalScoreP1: game.Player1Score,
// 		FinalScoreP2: game.Player2Score,
// 		CoinsEarned: game.Player1Score*5 + (func() int {
// 			if winner == p1.Login {
// 				return 20
// 			}
// 			return 0
// 		}()),
// 	}
// 	publishToPlayer(sala.ID, p1.Login, protocolo.Message{Type: "GAME_OVER", Data: gameOverMsgP1})

// 	gameOverMsgP2 := protocolo.GameOverMessage{
// 		Winner:       winner,
// 		FinalScoreP1: game.Player1Score,
// 		FinalScoreP2: game.Player2Score,
// 		CoinsEarned: game.Player2Score*5 + (func() int {
// 			if winner == p2.Login {
// 				return 20
// 			}
// 			return 0
// 		}()),
// 	}
// 	publishToPlayer(sala.ID, p2.Login, protocolo.Message{Type: "GAME_OVER", Data: gameOverMsgP2})

// 	mu.Lock()
// 	delete(playersInRoom, sala.Jogador1.RemoteAddr().String())
// 	if sala.Jogador2 != nil {
// 		delete(playersInRoom, sala.Jogador2.RemoteAddr().String())
// 	}
// 	delete(salas, sala.ID)
// 	mu.Unlock()
// }

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
                                        // ^-- Note: localRand aqui é o fsmRand quando chamado pela FSM
    instrument := GetRandomInstrumentByRarity(localRand, rarity)
    if instrument == nil {
        return nil
    }

    // Gera o ID único e DETERMINÍSTICO usando o rand passado (fsmRand)
    uniqueID := randomGenerateDeterministic(localRand, 4)

    // "Carimba" o ID único no instrumento
    instrument.ID = uniqueID

    // Retorna o pacote usando o mesmo ID
    return &protocolo.Packet{
        ID:         uniqueID,
        Rarity:     rarity,
        Instrument: *instrument,
        Opened:     false,
    }
}

func initializePacketStock() {
    // Cria um gerador de números aleatórios com uma semente FIXA.
    localRand := rand.New(rand.NewSource(12345)) // Semente fixa

    packetStock = make(map[string]*protocolo.Packet)
    rarities := []string{"Comum", "Raro", "Épico", "Lendário"}
    for _, rarity := range rarities {
        for i := 0; i < 50; i++ {
            instrument := GetRandomInstrumentByRarity(localRand, rarity) // Pega um instrumento base
            if instrument != nil {
                // Gera o ID único e DETERMINÍSTICO
                uniqueID := randomGenerateDeterministic(localRand, 4)

                // "Carimba" o ID único no instrumento
                instrument.ID = uniqueID

                // Cria o pacote usando o mesmo ID único
                packet := &protocolo.Packet{
                    ID:         uniqueID, // ID do pacote == ID do instrumento
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

		if player == nil {
			sendScreenMsg(conn, "Erro: jogador não encontrado.")
			return true
		}
		
		// Pega o ID do instrumento selecionado (precisa ser por ID, não por índice)
		var selectedInstrumentID string
		if req.InstrumentoID >= 0 && req.InstrumentoID < len(player.Inventario.Instrumentos) {
			selectedInstrumentID = player.Inventario.Instrumentos[req.InstrumentoID].ID
		} else {
			sendScreenMsg(conn, "Seleção de instrumento inválida.")
			return true
		}

		// Prepara o payload para a FSM
		payload := map[string]interface{}{
			"player_login":   player.Login,
			"instrumento_id": selectedInstrumentID,
		}

		// --- Lógica Líder/Seguidor ---
		if raftNode.State() != raft.Leader {
			// Seguidor: Encaminha para o líder
			forwardRequestToLeader("/request-select-instrument", payload, conn)
		} else {
			// Líder: Aplica no RAFT
			cmdPayload, _ := json.Marshal(payload)
			cmd := []byte("SELECT_INSTRUMENT_RAFT:" + string(cmdPayload))

			future := raftNode.Apply(cmd, 500*time.Millisecond)
			if err := future.Error(); err != nil {
				logger.Printf(ColorRed+"Erro RAFT ao selecionar instrumento por %s: %v"+ColorReset, player.Login, err)
				sendScreenMsg(conn, "Erro interno ao selecionar (RAFT).")
				return true
			}
			fsmResp := future.Response().(fsmApplyResponse)
			if fsmResp.err != nil {
				sendScreenMsg(conn, fmt.Sprintf("Erro FSM: %v", fsmResp.err))
				return true
			}
			
			// A FSM responde com "Selected:NomeDoInstrumento" ou um erro
			if respStr, ok := fsmResp.response.(string); ok && strings.HasPrefix(respStr, "Selected:") {
				instrumentName := strings.TrimPrefix(respStr, "Selected:")
				sendScreenMsg(conn, fmt.Sprintf("Instrumento '%s' selecionado para a batalha!", instrumentName))
			} else {
				sendScreenMsg(conn, fmt.Sprintf("Não foi possível selecionar: %s", fsmResp.response))
			}
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

	mux.HandleFunc("/request-create-room", func(w http.ResponseWriter, r *http.Request) {
        if raftNode.State() != raft.Leader { http.Error(w, "Não sou o líder.", http.StatusServiceUnavailable); return }
        var reqBody map[string]interface{}
        if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil { http.Error(w, "Corpo inválido", http.StatusBadRequest); return }
        playerLogin, _ := reqBody["player_login"].(string)
        if playerLogin == "" { http.Error(w, "Missing player_login", http.StatusBadRequest); return }

        logger.Printf(ColorCyan+"[API REST] Recebido pedido para criar sala para %s"+ColorReset, playerLogin)

        // Líder gera ID e aplica RAFT
         roomID := randomGenerate(6)
         cmdPayload, _ := json.Marshal(map[string]interface{}{
             "room_id":      roomID,
             "player1_login": playerLogin,
             "is_private":   true, // /request-create-room é sempre privado
         })
         cmd := []byte("CREATE_ROOM_RAFT:" + string(cmdPayload))
         future := raftNode.Apply(cmd, 500*time.Millisecond)

         if err := future.Error(); err != nil {
              http.Error(w, "Erro interno RAFT", http.StatusInternalServerError)
              return
         }
          fsmResp := future.Response().(fsmApplyResponse)
          if fsmResp.err != nil {
               http.Error(w, fmt.Sprintf("Erro FSM: %v", fsmResp.err), http.StatusInternalServerError)
               return
          }
          if fsmResp.response != "RoomCreated" {
                http.Error(w, fmt.Sprintf("Não foi possível criar sala: %s", fsmResp.response), http.StatusBadRequest)
               return
          }

         // Sucesso: Retorna a mensagem que o cliente espera
         w.Header().Set("Content-Type", "application/json")
         w.WriteHeader(http.StatusOK)
         respMsg := protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: "Sala privada criada. Código: "+roomID}}
         json.NewEncoder(w).Encode(respMsg)
    })

     mux.HandleFunc("/request-find-room", func(w http.ResponseWriter, r *http.Request) {
         if raftNode.State() != raft.Leader { http.Error(w, "Não sou o líder.", http.StatusServiceUnavailable); return }
         var reqBody map[string]interface{}
         if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil { http.Error(w, "Corpo inválido", http.StatusBadRequest); return }
         playerLogin, _ := reqBody["player_login"].(string)
         mode, _ := reqBody["mode"].(string)
         roomCode, _ := reqBody["room_code"].(string)
         if playerLogin == "" { http.Error(w, "Missing player_login", http.StatusBadRequest); return }

         logger.Printf(ColorCyan+"[API REST] Recebido pedido para encontrar sala para %s (Modo: %s, Código: %s)"+ColorReset, playerLogin, mode, roomCode)

         // Lógica do Líder (similar a findRoom, mas aplica RAFT)
         var applyFuture raft.ApplyFuture
         targetRoomID := "" // Usado para handleJoinRoomResultREST

          if mode == "PUBLIC" {
              mu.Lock()
              waitingRoomExists := len(salasEmEspera) > 0
              if waitingRoomExists { targetRoomID = salasEmEspera[0].ID }
              mu.Unlock()

              if waitingRoomExists {
                   cmdPayload, _ := json.Marshal(map[string]interface{}{ "room_id": targetRoomID, "player2_login": playerLogin})
                   cmd := []byte("JOIN_ROOM_RAFT:" + string(cmdPayload))
                   applyFuture = raftNode.Apply(cmd, 500*time.Millisecond)
              } else {
                   roomID := randomGenerate(6)
                   targetRoomID = roomID
                   cmdPayload, _ := json.Marshal(map[string]interface{}{ "room_id": roomID, "player1_login": playerLogin, "is_private": false})
                   cmd := []byte("CREATE_ROOM_RAFT:" + string(cmdPayload))
                   applyFuture = raftNode.Apply(cmd, 500*time.Millisecond)

                   // Tratamento especial para criação de sala pública (resposta imediata)
                    if err := applyFuture.Error(); err != nil { http.Error(w, "Erro RAFT", 500); return }
                    fsmResp := applyFuture.Response().(fsmApplyResponse)
                    if fsmResp.err != nil { http.Error(w, fmt.Sprintf("Erro FSM: %v", fsmResp.err), 500); return }
                    if fsmResp.response != "RoomCreated" { http.Error(w, fmt.Sprintf("Não foi possível criar sala: %s", fsmResp.response), 400); return }

                    // Resposta para sala pública criada (cliente fica esperando)
                    respMsg := protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: "Aguardando outro jogador na sala pública..."}}
                    w.Header().Set("Content-Type", "application/json")
                    json.NewEncoder(w).Encode(respMsg)
                    return // Encerra aqui para criação pública
              }
          } else if roomCode != "" {
                targetRoomID = roomCode
                cmdPayload, _ := json.Marshal(map[string]interface{}{ "room_id": roomCode, "player2_login": playerLogin})
                cmd := []byte("JOIN_ROOM_RAFT:" + string(cmdPayload))
                applyFuture = raftNode.Apply(cmd, 500*time.Millisecond)
          } else {
               http.Error(w, "Modo ou código inválido", http.StatusBadRequest)
               return
          }

         // Trata o resultado do JOIN (público ou privado) via REST
         handleJoinRoomResultREST(applyFuture, w, playerLogin, targetRoomID)
     })

    mux.HandleFunc("/request-play-note", func(w http.ResponseWriter, r *http.Request) {
        if raftNode.State() != raft.Leader { http.Error(w, "Não sou o líder.", http.StatusServiceUnavailable); return }
        var reqBody map[string]interface{}
        if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil { http.Error(w, "Corpo inválido", http.StatusBadRequest); return }
        playerLogin, _ := reqBody["player_login"].(string)
        note, _ := reqBody["note"].(string)
        if playerLogin == "" || note == "" { http.Error(w, "Dados ausentes", http.StatusBadRequest); return }

        logger.Printf(ColorCyan+"[API REST] Recebido pedido para jogar nota %s de %s"+ColorReset, note, playerLogin)

        // Aplica no RAFT
         cmdPayload, _ := json.Marshal(map[string]interface{}{
             "player_login": playerLogin,
             "note":         note,
         })
         cmd := []byte("PLAY_NOTE_RAFT:" + string(cmdPayload))
         future := raftNode.Apply(cmd, 500*time.Millisecond)

         if err := future.Error(); err != nil { http.Error(w, "Erro RAFT", 500); return }
         fsmResp := future.Response().(fsmApplyResponse)
          if fsmResp.err != nil { http.Error(w, fmt.Sprintf("Erro FSM: %v", fsmResp.err), 500); return }

          // Processa o resultado da FSM
          var responseToFollower interface{}

          switch resultData := fsmResp.response.(type) {
          case string: // Erro de negócio
               responseToFollower = protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: fmt.Sprintf("Não foi possível jogar: %s", resultData)}}
          case map[string]interface{}: // Sucesso
                p1Login := resultData["p1Login"].(string)
                p2Login := resultData["p2Login"].(string)
                salaID := ""
                mu.Lock()
                sala, ok := playersInRoom[playerLogin]
                if ok && sala != nil { salaID = sala.ID }
                currentP1Score, currentP2Score := 0, 0
                if ok && sala != nil && sala.Game != nil {
                    currentP1Score = sala.Game.Player1Score
                    currentP2Score = sala.Game.Player2Score
                }
                mu.Unlock()

                if salaID != "" {
                     // Envia ROUND_RESULT MQTT
                     if roundResultData, ok := resultData["roundResult"]; ok {
                          var roundResult protocolo.RoundResultMessage
                          mapToStruct(roundResultData, &roundResult)
                          resultMsgP1 := roundResult; resultMsgP1.YourScore = currentP1Score; resultMsgP1.OpponentScore = currentP2Score
                          publishToPlayer(salaID, p1Login, protocolo.Message{Type: "ROUND_RESULT", Data: resultMsgP1})
                          resultMsgP2 := roundResult; resultMsgP2.YourScore = currentP2Score; resultMsgP2.OpponentScore = currentP1Score
                          publishToPlayer(salaID, p2Login, protocolo.Message{Type: "ROUND_RESULT", Data: resultMsgP2})
                     }
                     // Verifica Game Over e envia MQTTs
                     if gameEnded, _ := resultData["gameEnded"].(bool); gameEnded {
                          winner := resultData["winner"].(string)
                           mu.Lock()
                           finalP1Score, finalP2Score := 0, 0
                           sala, salaOk := salas[salaID]
                           if salaOk && sala != nil && sala.Game != nil {
                               finalP1Score = sala.Game.Player1Score
                               finalP2Score = sala.Game.Player2Score
                           }
                           mu.Unlock()
                           p1CoinsEarned := finalP1Score*5 + (func() int { if winner == p1Login { return 20 }; return 0 }())
                           p2CoinsEarned := finalP2Score*5 + (func() int { if winner == p2Login { return 20 }; return 0 }())
                           gameOverMsgP1 := protocolo.GameOverMessage{ Winner: winner, FinalScoreP1: finalP1Score, FinalScoreP2: finalP2Score, CoinsEarned: p1CoinsEarned }
                           publishToPlayer(salaID, p1Login, protocolo.Message{Type: "GAME_OVER", Data: gameOverMsgP1})
                           gameOverMsgP2 := protocolo.GameOverMessage{ Winner: winner, FinalScoreP1: finalP1Score, FinalScoreP2: finalP2Score, CoinsEarned: p2CoinsEarned }
                           publishToPlayer(salaID, p2Login, protocolo.Message{Type: "GAME_OVER", Data: gameOverMsgP2})
                           // Líder limpa estado da sala
                           mu.Lock()
                           delete(playersInRoom, p1Login)
                           delete(playersInRoom, p2Login)
                           delete(salas, salaID)
                           mu.Unlock()
                     } else {
						// Envia TURN_UPDATE MQTT
						nextTurnPlayerInt, ok := resultData["nextTurn"].(int)
						if !ok {
						// Se não for int, TENTA float64
							if nextTurnPlayerFloat, okFloat := resultData["nextTurn"].(float64); okFloat {
								nextTurnPlayerInt = int(nextTurnPlayerFloat)
							} else {
							// Não dá panic, só loga o erro. A resposta HTTP ainda precisa ser enviada.
								logger.Printf(ColorRed+"[API REST] Erro CRÍTICO: 'nextTurn' não é nem int nem float64. Tipo: %T"+ColorReset, resultData["nextTurn"])
							}
						}
						nextTurnPlayer := nextTurnPlayerInt // Usa o valor corrigido

						publishToPlayer(salaID, p1Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: nextTurnPlayer == 1}})
						publishToPlayer(salaID, p2Login, protocolo.Message{Type: "TURN_UPDATE", Data: protocolo.TurnMessage{IsYourTurn: nextTurnPlayer == 2}})
					}
                }
                responseToFollower = protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: "Nota jogada."}} // Confirmação simples
          default:
              responseToFollower = protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: "Erro inesperado ao processar nota."}}
          }

           w.Header().Set("Content-Type", "application/json")
           json.NewEncoder(w).Encode(responseToFollower) // Envia a resposta HTTP de volta
    })

	mux.HandleFunc("/request-select-instrument", func(w http.ResponseWriter, r *http.Request) {
		if raftNode.State() != raft.Leader { http.Error(w, "Não sou o líder.", http.StatusServiceUnavailable); return }
		
		var reqBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil { http.Error(w, "Corpo inválido", http.StatusBadRequest); return }
		playerLogin, _ := reqBody["player_login"].(string)
		
		logger.Printf(ColorCyan+"[API REST] Recebido pedido para selecionar instrumento de %s"+ColorReset, playerLogin)

		// Líder: Aplica no RAFT
		cmdPayload, _ := json.Marshal(reqBody)
		cmd := []byte("SELECT_INSTRUMENT_RAFT:" + string(cmdPayload))
		future := raftNode.Apply(cmd, 500*time.Millisecond)

		if err := future.Error(); err != nil {
			http.Error(w, "Erro RAFT", 500)
			return
		}
		fsmResp := future.Response().(fsmApplyResponse)
		if fsmResp.err != nil {
			http.Error(w, fmt.Sprintf("Erro FSM: %v", fsmResp.err), 500)
			return
		}

		// Prepara a resposta de volta para o Seguidor
		var respMsg protocolo.Message
		if respStr, ok := fsmResp.response.(string); ok && strings.HasPrefix(respStr, "Selected:") {
			instrumentName := strings.TrimPrefix(respStr, "Selected:")
			respMsg = protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: fmt.Sprintf("Instrumento '%s' selecionado para a batalha!", instrumentName)}}
		} else {
			respMsg = protocolo.Message{Type: "SCREEN_MSG", Data: protocolo.ScreenMessage{Content: fmt.Sprintf("Não foi possível selecionar: %s", fsmResp.response)}}
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(respMsg)
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
	// loadPlayerData()
	if err := loadInstruments(); err != nil {
		logger.Fatal(err)
	}

	players = make(map[string]*User)
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

	go setupServerMQTTClient("tcp://broker1:1883", *nodeID)

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
	// go func() {
	// 	<-sigs
	// 	savePlayerData()
	// 	os.Exit(0)
	// }()

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

// setupServerMQTTClient conecta o SERVIDOR ao broker MQTT
func setupServerMQTTClient(brokerAddr string, nodeID string) {
    opts := mqtt.NewClientOptions()
    opts.AddBroker(brokerAddr)
    // ID único para o cliente MQTT do *servidor*
    opts.SetClientID(fmt.Sprintf("server-%s-%d", nodeID, time.Now().UnixNano()))
    opts.SetAutoReconnect(true)
    opts.SetConnectRetry(true)
    opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
        logger.Printf(ColorRed+"[MQTT-Servidor] Conexão perdida com o broker: %v"+ColorReset, err)
    })
    opts.SetOnConnectHandler(func(client mqtt.Client) {
        logger.Printf(ColorGreen+"[MQTT-Servidor] Conectado ao broker MQTT com sucesso!"+ColorReset)
    })

    mqttClient = mqtt.NewClient(opts)
    if token := mqttClient.Connect(); token.WaitTimeout(5*time.Second) && token.Error() != nil {
        logger.Fatalf(ColorRed+"Falha fatal ao conectar o servidor ao broker MQTT: %v"+ColorReset, token.Error())
    } else if token.Error() != nil {
        logger.Printf(ColorYellow+"[MQTT-Servidor] Aviso: %v"+ColorReset, token.Error())
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