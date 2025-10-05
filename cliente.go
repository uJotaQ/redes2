package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"pbl_redes/protocolo"
)

// Estados do cliente
type GameState int

const (
	LoginState GameState = iota
	MenuState
	WaitingState
	InGameState
	TurnState
	StopState
)

var (
	currentUser       string
	currentInventario protocolo.Inventario
	currentBalance    int
	currentState      GameState
	isMyTurn          bool
	mqttClient        mqtt.Client
)

// --- LÓGICA DE CONEXÃO E MQTT ---

// messageHandler é a função que será chamada toda vez que uma mensagem
// chegar em um dos tópicos que o cliente assinou.
func messageHandler(client mqtt.Client, msg mqtt.Message) {
	var genericMsg protocolo.Message
	if err := json.Unmarshal(msg.Payload(), &genericMsg); err != nil {
		fmt.Printf("Erro ao decodificar mensagem MQTT: %v\n", err)
		return
	}

	// Aqui movemos a lógica de tratamento de eventos de jogo que antes estava no interpreter
	switch genericMsg.Type {
	case "GAME_START":
		var data protocolo.GameStartMessage
		mapToStruct(genericMsg.Data, &data)
		fmt.Printf("\n--- ⚔️ BATALHA INICIADA vs %s ⚔️ ---\n", data.Opponent)
		currentState = InGameState
	case "TURN_UPDATE":
		var data protocolo.TurnMessage
		mapToStruct(genericMsg.Data, &data)
		isMyTurn = data.IsYourTurn
		if isMyTurn {
			currentState = TurnState
		} else {
			fmt.Println("Aguardando oponente...")
		}
	case "ROUND_RESULT":
		var data protocolo.RoundResultMessage
		mapToStruct(genericMsg.Data, &data)
		fmt.Printf("\n> %s jogou a nota: %s\n", data.PlayerName, data.PlayedNote)
		fmt.Printf("  Sequência atual: %s\n", data.CurrentSequence)
		if data.AttackTriggered {
			fmt.Printf("  💥 ATAQUE '%s' por %s!\n", data.AttackName, data.AttackerName)
		}
		fmt.Printf("  Placar: Você %d x %d Oponente\n", data.YourScore, data.OpponentScore)
	case "GAME_OVER":
		var data protocolo.GameOverMessage
		mapToStruct(genericMsg.Data, &data)
		fmt.Println("\n\n--- FIM DE JOGO ---")
		if data.Winner == currentUser {
			fmt.Println("🏆 VOCÊ VENCEU! 🏆")
		} else if data.Winner == "EMPATE" {
			fmt.Println("A partida terminou em EMPATE!")
		} else {
			fmt.Printf("💀 Você perdeu. O vencedor é: %s\n", data.Winner)
		}
		fmt.Printf("Você ganhou %d moedas!\n", data.CoinsEarned)
		currentBalance += data.CoinsEarned
		fmt.Println("Voltando para o menu principal em 5 segundos...")
		time.Sleep(5 * time.Second)
		currentState = MenuState
	}
}

func setupMQTTClient() {
	// Lista de endereços dos brokers
	brokerAddresses := []string{
		"tcp://127.0.0.1:1883",
		"tcp://127.0.0.1:1884", // Alterar os IPS pra teste depois
		"tcp://127.0.0.1:1885",
	}

	opts := mqtt.NewClientOptions()
	for _, addr := range brokerAddresses {
		opts.AddBroker(addr)
	}
	
	opts.SetClientID(fmt.Sprintf("client-%s-%d", currentUser, time.Now().Unix()))
	opts.SetDefaultPublishHandler(messageHandler)
	// Adiciona lógica de reconexão automática
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)

	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		fmt.Println("Erro fatal ao conectar a qualquer Broker MQTT:", token.Error())
		os.Exit(1)
	}
	fmt.Println("\n[INFO] Conexão MQTT estabelecida com sucesso.")
}

func subscribeToGameTopic(salaID, playerLogin string) {
	topic := fmt.Sprintf("game/%s/%s", salaID, playerLogin)
	if token := mqttClient.Subscribe(topic, 0, nil); token.Wait() && token.Error() != nil {
		fmt.Printf("Erro ao se inscrever no tópico do jogo: %v\n", token.Error())
		return
	}
	fmt.Printf("[INFO] Inscrito no tópico da partida: %s\n", topic)
}

// --- FUNÇÕES AUXILIARES ---

func sendJSON(writer *bufio.Writer, msg protocolo.Message) {
	jsonData, _ := json.Marshal(msg)
	writer.Write(jsonData)
	writer.WriteString("\n")
	writer.Flush()
}

func mapToStruct(input interface{}, target interface{}) error {
	bytes, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func readLine(reader *bufio.Reader) string {
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

// --- FUNÇÕES DE UI ---

func showMainMenu() {
	fmt.Println("\n--- MENU PRINCIPAL ---")
	fmt.Println("1. Entrar em Sala Pública")
	fmt.Println("2. Entrar em Sala Privada")
	fmt.Println("3. Criar Sala Privada")
	fmt.Println("4. Comprar Pacote de Instrumento (20 moedas)")
	fmt.Println("5. Meu Inventário")
	fmt.Println("6. Selecionar Instrumento para Batalha")
	fmt.Println("7. Consultar Saldo")
	fmt.Println("0. Sair")
	fmt.Print("> ")
}

func showLoginMenu() {
	fmt.Println("\n--- BEM-VINDO AO LUTHI BOX ---")
	fmt.Println("1. Login")
	fmt.Println("2. Cadastro")
	fmt.Println("0. Sair")
	fmt.Print("> ")
}

func showInventory() {
	if len(currentInventario.Instrumentos) == 0 {
		fmt.Println("\nSeu inventário está vazio.")
		return
	}
	fmt.Println("\n--- SEU INVENTÁRIO DE INSTRUMENTOS ---")
	for i, inst := range currentInventario.Instrumentos {
		fmt.Printf("\n%d) %s (%s)\n", i+1, inst.Name, inst.Rarity)
		fmt.Println("   Ataques:")
		for _, attack := range inst.Attacks {
			fmt.Printf("   - %s: %s\n", attack.Name, strings.Join(attack.Sequence, "-"))
		}
	}
	fmt.Println("------------------------------------")
}

// --- LÓGICA DO JOGO (CLIENT-SIDE) ---

func selectInstrument(writer *bufio.Writer, reader *bufio.Reader) {
	if len(currentInventario.Instrumentos) == 0 {
		fmt.Println("Você não tem instrumentos! Compre um pacote primeiro.")
		return
	}
	showInventory()
	fmt.Print("Digite o número do instrumento que deseja usar na batalha: ")
	input := readLine(reader)
	choice, err := strconv.Atoi(input)
	if err != nil || choice < 1 || choice > len(currentInventario.Instrumentos) {
		fmt.Println("Seleção inválida.")
		return
	}
	req := protocolo.SelectInstrumentRequest{InstrumentoID: choice - 1}
	sendJSON(writer, protocolo.Message{Type: "SELECT_INSTRUMENT", Data: req})
}

func handleGameTurn(writer *bufio.Writer, reader *bufio.Reader) {
	fmt.Print("Sua vez! Digite uma nota (A-G): ")
	note := readLine(reader)
	// A ação do jogador ainda é enviada via TCP para o servidor processar
	req := protocolo.PlayNoteRequest{Note: strings.ToUpper(note)}
	sendJSON(writer, protocolo.Message{Type: "PLAY_NOTE", Data: req})
	isMyTurn = false
	currentState = InGameState // Aguarda resultado via MQTT
}

// --- INTERPRETADOR DE MENSAGENS TCP ---

func interpreterTCP(conn net.Conn, writer *bufio.Writer, gameChannel chan protocolo.Message) {
	reader := bufio.NewReader(conn)
	for {
		message, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Println("\nConexão com o servidor perdida.")
			}
			os.Exit(0)
		}

		var msg protocolo.Message
		json.Unmarshal([]byte(message), &msg)

		// O channel agora envia a mensagem completa para a main thread
		gameChannel <- msg
	}
}

// --- FUNÇÃO MAIN ---

func main() {
	rand.Seed(time.Now().UnixNano())

	// Lista de servidores para o balanceamento de carga aleatório
	serverAddresses := []string{
		"127.0.0.1:8080",
		// "127.0.0.1:8081", // Adicione outros servidores aqui quando tiver
		// "127.0.0.1:8082",
	}

	var conn net.Conn
	var err error

	// Loop de retentativa e seleção aleatória
	for {
		// Escolhe um servidor aleatório da lista
		address := serverAddresses[rand.Intn(len(serverAddresses))]
		fmt.Printf("Tentando conectar ao servidor %s...\n", address)

		conn, err = net.Dial("tcp", address)
		if err == nil {
			// Se conectou, sai do loop
			break
		}
		fmt.Printf("Falha ao conectar: %v. Tentando outro em 2 segundos...\n", err)
		time.Sleep(2 * time.Second)
	}
	defer conn.Close()
	fmt.Printf("Conectado com sucesso ao servidor %s\n", conn.RemoteAddr())

	writer := bufio.NewWriter(conn)
	// O channel agora transporta a mensagem inteira, não apenas uma string
	gameChannel := make(chan protocolo.Message)
	go interpreterTCP(conn, writer, gameChannel)

	userInputReader := bufio.NewReader(os.Stdin)
	currentState = LoginState

	for {
		select {
		case msg := <-gameChannel:
			// A main thread agora processa todas as mensagens TCP
			switch msg.Type {
			case "LOGIN":
				var data protocolo.LoginResponse
				mapToStruct(msg.Data, &data)
				if data.Status == "LOGADO" {
					fmt.Println("Login bem-sucedido!")
					currentBalance = data.Saldo
					currentInventario = data.Inventario
					currentState = MenuState
					// Após logar, estabelece a conexão MQTT
					go setupMQTTClient()
				} else {
					fmt.Println("Falha no login.")
					currentState = LoginState
				}
			case "PAREADO":
				var data protocolo.PairingMessage
				mapToStruct(msg.Data, &data)
				currentState = InGameState
				fmt.Println("\nPartida encontrada! Aguarde o início...")
				// Após ser pareado, se inscreve no tópico específico do jogo
				subscribeToGameTopic(data.SalaID, data.PlayerLogin)
			case "SCREEN_MSG":
				var data protocolo.ScreenMessage
				mapToStruct(msg.Data, &data)
				fmt.Println("\n[SERVIDOR]: " + data.Content)
			case "COMPRA_RESPONSE":
				var data protocolo.CompraResponse
				mapToStruct(msg.Data, &data)
				if data.Status == "COMPRA_APROVADA" {
					fmt.Printf("\n🎉 Você conseguiu um novo instrumento: %s (%s)!\n", data.NovoInstrumento.Name, data.NovoInstrumento.Rarity)
					currentInventario = data.Inventario
				}
				currentState = MenuState
			case "BALANCE_RESPONSE":
				var data protocolo.BalanceResponse
				mapToStruct(msg.Data, &data)
				fmt.Printf("\nSeu saldo atual: %d moedas.\n", data.Saldo)
				currentBalance = data.Saldo
			}
		default:
			// Loop não bloqueante
		}

		// A máquina de estados continua igual, mas agora é controlada pela main thread
		switch currentState {
		case LoginState:
			showLoginMenu()
			choice := readLine(userInputReader)
			if choice == "1" {
				fmt.Print("Login: ")
				login := readLine(userInputReader)
				fmt.Print("Senha: ")
				senha := readLine(userInputReader)
				currentUser = login
				sendJSON(writer, protocolo.Message{Type: "LOGIN", Data: protocolo.LoginRequest{Login: login, Senha: senha}})
				currentState = StopState
			} else if choice == "2" {
				fmt.Print("Escolha um login: ")
				login := readLine(userInputReader)
				fmt.Print("Escolha uma senha: ")
				senha := readLine(userInputReader)
				sendJSON(writer, protocolo.Message{Type: "CADASTRO", Data: protocolo.SignInRequest{Login: login, Senha: senha}})
			} else if choice == "0" {
				sendJSON(writer, protocolo.Message{Type: "QUIT"})
				return
			}
		case MenuState:
			showMainMenu()
			choice := readLine(userInputReader)
			switch choice {
			case "1":
				sendJSON(writer, protocolo.Message{Type: "FIND_ROOM", Data: protocolo.RoomRequest{Mode: "PUBLIC"}})
				currentState = WaitingState
			case "2":
				fmt.Print("Digite o código da sala: ")
				code := readLine(userInputReader)
				sendJSON(writer, protocolo.Message{Type: "PRIV_ROOM", Data: protocolo.RoomRequest{RoomCode: code}})
				currentState = WaitingState
			case "3":
				sendJSON(writer, protocolo.Message{Type: "CREATE_ROOM"})
				currentState = WaitingState
			case "4":
				sendJSON(writer, protocolo.Message{Type: "COMPRA"})
				currentState = StopState
			case "5":
				showInventory()
			case "6":
				selectInstrument(writer, userInputReader)
			case "7":
				sendJSON(writer, protocolo.Message{Type: "CHECK_BALANCE"})
			case "0":
				sendJSON(writer, protocolo.Message{Type: "QUIT"})
				return
			default:
				fmt.Println("Opção inválida.")
			}
		case TurnState:
			handleGameTurn(writer, userInputReader)
		case WaitingState, InGameState, StopState:
			time.Sleep(200 * time.Millisecond)
		}
	}
}