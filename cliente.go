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

	"pbl_redes/protocolo"

	mqtt "github.com/eclipse/paho.mqtt.golang"
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

func messageHandler(client mqtt.Client, msg mqtt.Message) {
	var genericMsg protocolo.Message
	if err := json.Unmarshal(msg.Payload(), &genericMsg); err != nil {
		fmt.Printf("Erro ao decodificar mensagem MQTT: %v\n", err)
		return
	}

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
		currentBalance += data.CoinsEarned // Atualiza saldo local
		fmt.Println("Voltando para o menu principal em 5 segundos...")
		time.Sleep(5 * time.Second)
		currentState = MenuState
	}
}

func setupMQTTClient() {
	opts := mqtt.NewClientOptions()
	// Conecta ao broker exposto pelo Docker no localhost
	opts.AddBroker("tcp://127.0.0.1:1883")
	// Cria um ID único para evitar conflitos se rodar múltiplos clientes
	opts.SetClientID(fmt.Sprintf("client-%s-%d", currentUser, time.Now().UnixNano()))
	opts.SetDefaultPublishHandler(messageHandler)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	// Define um callback para quando a conexão for perdida
	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		fmt.Printf("\n[MQTT] Conexão perdida: %v. Tentando reconectar...\n", err)
	})
	// Define um callback para quando a conexão for reestabelecida
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		fmt.Println("\n[MQTT] Conexão reestabelecida!")
		// Se o usuário estiver logado, poderia tentar se reinscrever nos tópicos aqui,
		// mas a lógica atual de re-inscrição após PAREADO é suficiente por enquanto.
	})

	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.WaitTimeout(5*time.Second) && token.Error() != nil {
		fmt.Println("[AVISO] Não foi possível conectar ao Broker MQTT:", token.Error())
		// Não encerra o programa, o cliente tentará reconectar em background
	} else if token.Error() == nil {
		fmt.Println("\n[INFO] Conexão MQTT inicial estabelecida.")
	}
}

func subscribeToGameTopic(salaID, playerLogin string) {
	if mqttClient == nil || !mqttClient.IsConnected() {
		fmt.Println("[AVISO] Cliente MQTT não conectado. Não foi possível se inscrever no tópico do jogo.")
		return
	}
	topic := fmt.Sprintf("game/%s/%s", salaID, playerLogin)
	// Usa QOS 1 para maior garantia de entrega das mensagens de jogo
	if token := mqttClient.Subscribe(topic, 1, nil); token.WaitTimeout(3*time.Second) && token.Error() != nil {
		fmt.Printf("[ERRO] Falha ao se inscrever no tópico do jogo %s: %v\n", topic, token.Error())
	} else if token.Error() == nil {
		fmt.Printf("[INFO] Inscrito no tópico da partida: %s\n", topic)
	}
}

// Desinscreve do tópico no final do jogo ou desconexão
func unsubscribeFromGameTopic(salaID, playerLogin string) {
	if mqttClient != nil && mqttClient.IsConnected() {
		topic := fmt.Sprintf("game/%s/%s", salaID, playerLogin)
		if token := mqttClient.Unsubscribe(topic); token.WaitTimeout(3*time.Second) && token.Error() != nil {
			fmt.Printf("[ERRO] Falha ao desinscrever do tópico %s: %v\n", topic, token.Error())
		} else if token.Error() == nil {
			fmt.Printf("[INFO] Desinscrito do tópico da partida: %s\n", topic)
		}
	}
}

// --- FUNÇÕES AUXILIARES ---

func sendJSON(writer *bufio.Writer, msg protocolo.Message) error {
	if writer == nil {
		return fmt.Errorf("writer é nil")
	}
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("erro ao codificar JSON: %v", err)
	}
	if _, err := writer.Write(jsonData); err != nil {
		return fmt.Errorf("erro ao escrever dados: %v", err)
	}
	if _, err := writer.WriteString("\n"); err != nil {
		return fmt.Errorf("erro ao escrever newline: %v", err)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("erro ao fazer flush: %v", err)
	}
	return nil
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
	if err := sendJSON(writer, protocolo.Message{Type: "SELECT_INSTRUMENT", Data: req}); err != nil {
		fmt.Printf("[ERRO] Falha ao enviar seleção de instrumento: %v\n", err)
	}

}

func handleGameTurn(writer *bufio.Writer, reader *bufio.Reader) {
	fmt.Print("Sua vez! Digite uma nota (A-G): ")
	note := readLine(reader)
	req := protocolo.PlayNoteRequest{Note: strings.ToUpper(note)}
	if err := sendJSON(writer, protocolo.Message{Type: "PLAY_NOTE", Data: req}); err != nil {
		fmt.Printf("[ERRO] Falha ao enviar nota: %v\n", err)
		// Se falhar ao enviar a nota, talvez o jogador deva voltar ao menu?
		// Por enquanto, apenas mudamos o estado.
	}

	isMyTurn = false
	currentState = InGameState // Volta a aguardar mensagens MQTT
}

// --- INTERPRETADOR DE MENSAGENS TCP ---

func interpreterTCP(conn net.Conn, gameChannel chan protocolo.Message) {
	// Garante que o canal seja fechado ao sair da função, sinalizando a desconexão
	defer close(gameChannel)

	reader := bufio.NewReader(conn)
	for {
		message, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Println("\n\n[INFO] Conexão com o servidor foi fechada.")
			} else {
				// Mostra o erro específico se não for EOF
				fmt.Printf("\n\n[ERRO] Erro na leitura da conexão TCP: %v\n", err)
			}
			return // Encerra a goroutine e o defer fecha o canal
		}

		var msg protocolo.Message
		if err := json.Unmarshal([]byte(message), &msg); err != nil {
			fmt.Printf("[AVISO] Recebida mensagem TCP inválida: %v\n", err)
			continue // Ignora a mensagem inválida e continua lendo
		}
		// Envia a mensagem válida para o canal
		gameChannel <- msg
	}
}

// --- FUNÇÃO MAIN ---

func connectToServer(addresses []string) net.Conn {
	var conn net.Conn
	var err error

	rand.Shuffle(len(addresses), func(i, j int) {
		addresses[i], addresses[j] = addresses[j], addresses[i]
	})

	for _, address := range addresses {
		fmt.Printf("Tentando conectar ao servidor %s...\n", address)
		conn, err = net.DialTimeout("tcp", address, 3*time.Second)
		if err == nil {
			fmt.Printf("Conectado com sucesso ao servidor %s\n", conn.RemoteAddr())
			return conn
		}
		fmt.Printf("Falha ao conectar em %s: %v\n", address, err)
	}
	return nil
}

func main() {
	rand.Seed(time.Now().UnixNano())

	serverAddresses := []string{
		"127.0.0.1:8081",
		"127.0.0.1:8082",
		"127.0.0.1:8083",
	}

	userInputReader := bufio.NewReader(os.Stdin)

ReconnectLoop:
	for {
		conn := connectToServer(serverAddresses)
		if conn == nil {
			fmt.Println("Nenhum servidor disponível. Tentando novamente em 5 segundos...")
			time.Sleep(5 * time.Second)
			continue ReconnectLoop
		}

		writer := bufio.NewWriter(conn)
		gameChannel := make(chan protocolo.Message)
		// Inicia a goroutine para ler da conexão
		go interpreterTCP(conn, gameChannel)

		// Reseta o estado apenas se for a primeira conexão
		if currentUser == "" {
			currentState = LoginState
		} else {
			// Se já estava logado, talvez volte ao Menu? Ou tente um Reconnect?
			// Por enquanto, vamos manter o estado, mas pode precisar de lógica de re-autenticação.
			fmt.Println("[INFO] Reconectado. Voltando ao menu principal.")
			currentState = MenuState // Volta ao menu após reconectar
		}

		// Loop principal do jogo/aplicação
		for {
			select {
			case msg, ok := <-gameChannel:
				if !ok { // Canal foi fechado pela interpreterTCP (desconexão)
					fmt.Println("\n[INFO] Conexão perdida. Iniciando processo de reconexão...")
					// Não precisa fechar conn aqui, DialTimeout já falhou ou interpreterTCP retornou
					time.Sleep(2 * time.Second) // Pequena pausa antes de tentar reconectar
					continue ReconnectLoop      // Volta ao loop externo para tentar nova conexão
				}

				// Processa mensagens recebidas do servidor via TCP
				switch msg.Type {
				case "LOGIN":
					var data protocolo.LoginResponse
					mapToStruct(msg.Data, &data)
					if data.Status == "LOGADO" {
						fmt.Println("Login bem-sucedido!")
						currentBalance = data.Saldo
						currentInventario = data.Inventario
						currentState = MenuState
						// Inicia a conexão MQTT APÓS o login TCP bem-sucedido
						go setupMQTTClient()
					} else if data.Status == "ONLINE_JA" {
						fmt.Println("Login falhou: Usuário já está online em outra sessão.")
						currentState = LoginState
					} else if data.Status == "N_EXIST" {
						fmt.Println("Login falhou: Usuário não existe.")
						currentState = LoginState
					} else {
						fmt.Println("Login falhou: Senha incorreta ou erro desconhecido.")
						currentState = LoginState
					}
				case "PAREADO":
					var data protocolo.PairingMessage
					mapToStruct(msg.Data, &data)
					if data.Status == "PAREADO" {
						currentState = InGameState // Muda para InGameState ao ser pareado
						fmt.Println("\nPartida encontrada! Aguarde o início via MQTT...")
						// Se inscreve no tópico MQTT após confirmação de pareamento
						subscribeToGameTopic(data.SalaID, data.PlayerLogin)
					} else {
						// Pode haver outros status de pareamento? (ex: FALHOU)
						fmt.Println("\n[AVISO] Falha no pareamento ou status desconhecido:", data.Status)
						currentState = MenuState // Volta ao menu se o pareamento falhar
					}
				case "SCREEN_MSG":
					var data protocolo.ScreenMessage
					mapToStruct(msg.Data, &data)
					fmt.Println("\n[SERVIDOR]: " + data.Content)
				case "COMPRA_RESPONSE":
					var data protocolo.CompraResponse
					mapToStruct(msg.Data, &data)
					switch data.Status {
					case "COMPRA_APROVADA":
						fmt.Printf("\n🎉 Você conseguiu um novo instrumento: %s (%s)!\n", data.NovoInstrumento.Name, data.NovoInstrumento.Rarity)
						currentInventario = data.Inventario // Atualiza inventário local
						currentBalance -= 20                // Assume que custou 20, idealmente o servidor confirmaria o novo saldo
						fmt.Printf("Saldo atual: %d moedas.\n", currentBalance)
					case "NO_BALANCE":
						fmt.Println("\nCompra falhou: Saldo insuficiente.")
					case "EMPTY_STORAGE":
						fmt.Println("\nCompra falhou: Loja sem estoque no momento.")
					case "RAFT_ERROR":
						fmt.Println("\nCompra falhou: Erro interno do servidor. Tente novamente.")
					default:
						fmt.Println("\nResposta da compra desconhecida:", data.Status)
					}
					currentState = MenuState // Volta ao menu após a tentativa de compra
				case "BALANCE_RESPONSE":
					var data protocolo.BalanceResponse
					mapToStruct(msg.Data, &data)
					fmt.Printf("\nSeu saldo atual: %d moedas.\n", data.Saldo)
					currentBalance = data.Saldo // Atualiza saldo local
				default:
					fmt.Printf("\n[AVISO] Mensagem TCP de tipo desconhecido recebida: %s\n", msg.Type)
				}

			default:
				// Executa a lógica da máquina de estados apenas se não houver mensagem no canal
				// Evita processar input do usuário enquanto processa mensagem do servidor

				// --- Máquina de Estados para Input do Usuário ---
				switch currentState {
				case LoginState:
					showLoginMenu()
					choice := readLine(userInputReader)
					if choice == "1" {
						fmt.Print("Login: ")
						login := readLine(userInputReader)
						fmt.Print("Senha: ")
						senha := readLine(userInputReader)
						currentUser = login // Guarda o login ANTES de enviar
						if err := sendJSON(writer, protocolo.Message{Type: "LOGIN", Data: protocolo.LoginRequest{Login: login, Senha: senha}}); err != nil {
							fmt.Printf("[ERRO] Falha ao enviar login: %v\n", err)
							// Se falhar ao enviar, talvez devesse tentar reconectar?
							// Por enquanto, apenas continua no estado de login.
							currentState = LoginState
						} else {
							currentState = StopState // Aguarda resposta do servidor
						}
					} else if choice == "2" {
						fmt.Print("Escolha um login: ")
						login := readLine(userInputReader)
						fmt.Print("Escolha uma senha: ")
						senha := readLine(userInputReader)
						if err := sendJSON(writer, protocolo.Message{Type: "CADASTRO", Data: protocolo.SignInRequest{Login: login, Senha: senha}}); err != nil {
							fmt.Printf("[ERRO] Falha ao enviar cadastro: %v\n", err)
						}
						// Continua no LoginState após tentar cadastrar
					} else if choice == "0" {
						fmt.Println("Saindo...")
						if err := sendJSON(writer, protocolo.Message{Type: "QUIT"}); err != nil {
							fmt.Printf("[AVISO] Falha ao enviar QUIT: %v\n", err)
						}
						time.Sleep(100 * time.Millisecond) // Pequena pausa
						conn.Close()                       // Fecha a conexão localmente
						return                             // Encerra o programa
					} else {
						fmt.Println("Opção inválida.")
					}
				case MenuState:
					showMainMenu()
					choice := readLine(userInputReader)
					var sendErr error
					switch choice {
					case "1":
						sendErr = sendJSON(writer, protocolo.Message{Type: "FIND_ROOM", Data: protocolo.RoomRequest{Mode: "PUBLIC"}})
						if sendErr == nil {
							currentState = WaitingState
						}
					case "2":
						fmt.Print("Digite o código da sala: ")
						code := readLine(userInputReader)
						sendErr = sendJSON(writer, protocolo.Message{Type: "PRIV_ROOM", Data: protocolo.RoomRequest{RoomCode: code}})
						if sendErr == nil {
							currentState = WaitingState
						}
					case "3":
						sendErr = sendJSON(writer, protocolo.Message{Type: "CREATE_ROOM"})
						if sendErr == nil {
							currentState = WaitingState
						}
					case "4":
						sendErr = sendJSON(writer, protocolo.Message{Type: "COMPRA"})
						if sendErr == nil {
							currentState = StopState
						} // Aguarda resposta da compra
					case "5":
						showInventory()
					case "6":
						selectInstrument(writer, userInputReader) // Envio é feito dentro da função
					case "7":
						sendErr = sendJSON(writer, protocolo.Message{Type: "CHECK_BALANCE"})
						// Continua no MenuState após pedir saldo
					case "0":
						fmt.Println("Saindo...")
						sendErr = sendJSON(writer, protocolo.Message{Type: "QUIT"})
						time.Sleep(100 * time.Millisecond) // Pequena pausa
						conn.Close()                       // Fecha a conexão localmente
						// Se o envio falhou, ainda sai
						return // Encerra o programa
					default:
						fmt.Println("Opção inválida.")
					}
					// Se houve erro ao enviar a mensagem, informa o usuário e permanece no menu
					if sendErr != nil {
						fmt.Printf("[ERRO] Falha ao enviar comando para o servidor: %v\n", sendErr)
						currentState = MenuState // Garante que volta ao menu
					}

				case TurnState:
					handleGameTurn(writer, userInputReader) // Envio é feito dentro da função

				case WaitingState, InGameState, StopState:
					// Estados onde o cliente principalmente espera por mensagens (TCP ou MQTT)
					// Adiciona um pequeno sleep para evitar uso excessivo de CPU no loop default
					time.Sleep(100 * time.Millisecond)
				}
				// Fim da máquina de estados
			} // Fim do select
		} // Fim do GameProcessingLoop
	} // Fim do ReconnectLoop
}
