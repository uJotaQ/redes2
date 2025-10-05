package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

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
)

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

// --- FUNÇÕES DE UI (INTERFACE DO USUÁRIO) ---

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
	req := protocolo.PlayNoteRequest{Note: strings.ToUpper(note)}
	sendJSON(writer, protocolo.Message{Type: "PLAY_NOTE", Data: req})
	isMyTurn = false
	currentState = InGameState // Aguarda resultado
}

// --- INTERPRETADOR DE MENSAGENS DO SERVIDOR ---

func interpreter(conn net.Conn, writer *bufio.Writer, gameChannel chan string) {
	reader := bufio.NewReader(conn)
	for {
		message, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Println("\nConexão com o servidor perdida.")
			}
			os.Exit(0)
			return
		}

		var msg protocolo.Message
		json.Unmarshal([]byte(message), &msg)

		switch msg.Type {
		case "LOGIN":
			var data protocolo.LoginResponse
			mapToStruct(msg.Data, &data)
			if data.Status == "LOGADO" {
				currentBalance = data.Saldo
				currentInventario = data.Inventario
			}
			gameChannel <- data.Status
		case "PAREADO":
			gameChannel <- "PAREADO"
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
			gameChannel <- data.Status
		case "BALANCE_RESPONSE":
			var data protocolo.BalanceResponse
			mapToStruct(msg.Data, &data)
			fmt.Printf("\nSeu saldo atual: %d moedas.\n", data.Saldo)
			currentBalance = data.Saldo
		case "GAME_START":
			var data protocolo.GameStartMessage
			mapToStruct(msg.Data, &data)
			fmt.Printf("\n--- ⚔️ BATALHA INICIADA vs %s ⚔️ ---\n", data.Opponent)
			currentState = InGameState
		case "TURN_UPDATE":
			var data protocolo.TurnMessage
			mapToStruct(msg.Data, &data)
			isMyTurn = data.IsYourTurn
			if isMyTurn {
				currentState = TurnState
			} else {
				fmt.Println("Aguardando oponente...")
			}
		case "ROUND_RESULT":
			var data protocolo.RoundResultMessage
			mapToStruct(msg.Data, &data)
			fmt.Printf("\n> %s jogou a nota: %s\n", data.PlayerName, data.PlayedNote)
			fmt.Printf("  Sequência atual: %s\n", data.CurrentSequence)
			if data.AttackTriggered {
				fmt.Printf("  💥 ATAQUE '%s' por %s!\n", data.AttackName, data.AttackerName)
			}
			fmt.Printf("  Placar: Você %d x %d Oponente\n", data.YourScore, data.OpponentScore)
		case "GAME_OVER":
			var data protocolo.GameOverMessage
			mapToStruct(msg.Data, &data)
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
			fmt.Printf("Placar Final: %d x %d\n", data.FinalScoreP1, data.FinalScoreP2)
			fmt.Println("Voltando para o menu principal em 5 segundos...")
			time.Sleep(5 * time.Second)
			currentState = MenuState
		}
	}
}

// --- FUNÇÃO MAIN ---

func main() {
	conn, err := net.Dial("tcp", "127.0.0.1:8080")
	if err != nil {
		fmt.Println("Erro ao conectar ao servidor:", err)
		return
	}
	defer conn.Close()
	fmt.Println("Conectado ao servidor!")

	writer := bufio.NewWriter(conn)
	userInputReader := bufio.NewReader(os.Stdin)
	gameChannel := make(chan string)

	go interpreter(conn, writer, gameChannel)

	currentState = LoginState

	for {
		select {
		case msg := <-gameChannel:
			switch msg {
			case "LOGADO":
				fmt.Println("Login bem-sucedido!")
				currentState = MenuState
			case "ONLINE_JA", "N_EXIST":
				fmt.Println("Falha no login.")
				currentState = LoginState
			case "PAREADO":
				currentState = InGameState
				fmt.Println("\nPartida encontrada! Aguarde o início...")
			case "COMPRA_APROVADA", "NO_BALANCE", "EMPTY_STORAGE":
				currentState = MenuState
			}
		default:
			// Non-blocking
		}

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