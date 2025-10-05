package protocolo

// Estrutura para Pacotes de Instrumentos
type Packet struct {
	ID         string      `json:"id"`
	Rarity     string      `json:"rarity"`
	Instrument Instrumento `json:"instrument"`
	Opened     bool        `json:"opened"`
}

// Estruturas de dados principais do jogo
type Attack struct {
	Name     string   `json:"name"`
	Sequence []string `json:"sequence"`
}

type Instrumento struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Rarity  string   `json:"rarity"`
	Attacks [3]Attack `json:"attacks"`
}

type Inventario struct {
	Instrumentos []Instrumento `json:"instrumentos"`
}

// Mensagem genérica para comunicação via socket
type Message struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// Estruturas para Login e Cadastro
type LoginRequest struct {
	Login string `json:"login"`
	Senha string `json:"senha"`
}

type LoginResponse struct {
	Status     string     `json:"status"`
	Inventario Inventario `json:"inventario"`
	Saldo      int        `json:"saldo"`
}

type SignInRequest struct {
	Login string `json:"login"`
	Senha string `json:"senha"`
}

// Mensagens de tela e chat
type ScreenMessage struct {
	Content string `json:"content"`
}

// Estruturas para Salas e Pareamento
type RoomRequest struct {
	RoomCode string `json:"room_code,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

type PairingMessage struct {
	Status      string `json:"status"`
	SalaID      string `json:"sala_id"`      // <-- ADICIONADO
	PlayerLogin string `json:"player_login"` // <-- ADICIONADO
}

// Estruturas para Compra de Pacotes
type OpenPackageRequest struct{}

type CompraResponse struct {
	Status          string       `json:"status"`
	NovoInstrumento *Instrumento `json:"novo_instrumento,omitempty"`
	Inventario      Inventario   `json:"inventario,omitempty"`
}

// Saldo e Latência
type CheckBalance struct{}

type BalanceResponse struct {
	Saldo int `json:"saldo"`
}

type LatencyRequest struct{}

type LatencyResponse struct {
	Latencia int64 `json:"latencia"`
}

// --- ESTRUTURAS PARA A PARTIDA MUSICAL ---

type SelectInstrumentRequest struct {
	InstrumentoID int `json:"instrumento_id"`
}

type GameStartMessage struct {
	Opponent string `json:"opponent"`
}

type RoundStartMessage struct {
	PlayerInstruments   []Instrumento `json:"player_instruments"`
	OpponentInstruments []Instrumento `json:"opponent_instruments"`
}

type PlayNoteRequest struct {
	Note string `json:"note"` // "A", "B", "C", etc.
}

type RoundResultMessage struct {
	PlayedNote      string `json:"played_note"`
	PlayerName      string `json:"player_name"`
	CurrentSequence string `json:"current_sequence"`
	AttackTriggered bool   `json:"attack_triggered"`
	AttackName      string `json:"attack_name,omitempty"`
	AttackerName    string `json:"attacker_name,omitempty"`
	YourScore       int    `json:"your_score"`
	OpponentScore   int    `json:"opponent_score"`
}

type TurnMessage struct {
	IsYourTurn bool `json:"is_your_turn"`
}

type GameOverMessage struct {
	Winner       string `json:"winner"`
	FinalScoreP1 int    `json:"final_score_p1"`
	FinalScoreP2 int    `json:"final_score_p2"`
	CoinsEarned  int    `json:"coins_earned"`
}