
# PBL_REDES - Jogo Rítmico Distribuído

**Autores**: Leonardo Oliveira Almeida da Cruz ([@oLeozito](https://github.com/oLeozito)) e João Gabriel Santos Silva ([@uJotaQ](https://github.com/uJotaQ))
**Disciplina**: TEC502 - Redes e Conectividade (PBL)

Este projeto implementa um servidor de jogo multiplayer distribuído, tolerante a falhas e escalável, como parte da avaliação da disciplina. O sistema foi projetado para migrar uma arquitetura de servidor de jogo centralizado para um cluster de nós distribuídos que gerenciam o estado do jogo de forma consistente.

---

## 🚀 Tecnologias Utilizadas

- **Go (Golang):** Linguagem principal para o desenvolvimento do servidor e cliente.  
- **Protocolo Raft:** Utilizado para consenso e replicação de estado (via biblioteca `hashicorp/raft`).  
- **TCP Sockets:** Protocolo de comunicação primário entre o cliente (`cliente.go`) e o nó do servidor (`servidor.go`) ao qual ele se conecta.  
- **API REST (HTTP):** Protocolo de comunicação *servidor-servidor*, usado para encaminhamento de requisições e broadcast de notificações.  
- **JSON:** Formato de serialização para todas as mensagens TCP e REST.  
- **Docker & Docker Compose:** Utilizado para orquestrar e testar o ambiente distribuído com múltiplos nós de servidor.

---

## 🏛️ Visão Geral da Arquitetura (Barema - Item 1)

A arquitetura do sistema é baseada no padrão de **Máquina de Estados Replicada (Replicated State Machine)**, utilizando o algoritmo de consenso Raft para garantir a consistência.

Diferente de uma arquitetura centralizada, onde um único servidor detém todo o estado (jogadores, salas, inventários), esta solução utiliza um *cluster* de nós `servidor.go`.

1. **Nós do Cluster:** Qualquer instância do `servidor.go` pode ser executada como um nó. Os nós se comunicam para eleger um **Líder**.  
2. **Líder vs. Seguidor:**
   - O **Líder** é o único nó autorizado a aplicar mudanças de estado (ex: cadastrar um jogador, comprar um pacote, jogar uma nota).  
   - Os **Seguidores (Followers)** atuam como *gateways* leves. Eles recebem conexões TCP dos clientes, mas, em vez de processar as lógicas de negócio, eles **encaminham** a requisição para o Líder via API REST.  
3. **Fluxo de Dados (FSM):**
   - Um cliente (ex: `cliente1`) envia um comando (ex: `COMPRAR_PACOTE`) via TCP para o nó ao qual está conectado (ex: `Servidor 2 - Seguidor`).  
   - O `Servidor 2` encaminha essa requisição para o `Servidor 1 - Líder` via REST (`/request-purchase`).  
   - O Líder recebe a requisição, a submete ao log do Raft (`raftNode.Apply()`).  
   - O Raft garante que este comando seja replicado e aplicado na **Máquina de Estados Finitos (FSM)** de *todos* os nós do cluster (incluindo o `Servidor 2`).  
   - A FSM (função `FSM.Apply`) executa a lógica de forma determinística, garantindo que o estoque de pacotes e o inventário do jogador sejam atualizados de forma idêntica em todos os nós.

Essa arquitetura permite **escalabilidade horizontal**: podemos adicionar mais nós seguidores para lidar com mais conexões de clientes, sem sobrecarregar o nó Líder, que se concentra apenas em orquestrar o estado.

---

## 📋 Respostas Detalhadas ao Barema

### 2. Comunicação Servidor-Servidor

A comunicação entre os nós do servidor é realizada através de duas vias principais:

1. **Raft (TCP):** O `hashicorp/raft` gerencia sua própria comunicação interna (binária) para eleição de líder e replicação de log.  
2. **API REST (HTTP):** Uma API REST (`startHttpApi`) é exposta por cada nó para permitir a colaboração e o encaminhamento de requisições.

**Endpoints REST Principais:**

- `/join`: (POST) Endpoint de *bootstrap*. Um novo nó o utiliza para solicitar ao Líder sua adição ao cluster Raft.  
- `/request-register`, `/request-login`, `/request-logout`, `/request-reconnect`: (POST) Usados por seguidores para encaminhar comandos de autenticação e sessão para o Líder.  
- `/request-purchase`, `/request-trade`: (POST) Usados por seguidores para encaminhar lógicas de negócio críticas (compra e troca) para o Líder.  
- `/request-create-room`, `/request-find-room`, `/request-play-note`: (POST) Usados por seguidores para encaminhar toda a lógica de pareamento e jogabilidade para o Líder.  
- `/notify-player`: (POST) Endpoint crucial usado pelo Líder para **transmitir (broadcast)** eventos a *todos* os seguidores.

---

### 3. Comunicação Cliente-Servidor

O barema sugere um modelo *publisher-subscriber*. A solução implementada atinge o *mesmo objetivo* (notificações *push* do servidor para o cliente) de forma mais integrada, usando uma combinação de TCP e REST.

- **Canal Primário (TCP):** A comunicação é bidirecional sobre uma única conexão TCP (`handleConnection`).  
- **Modelo "Pub/Sub" Simulado (Push):**
  1. O Líder (Publisher) decide que um evento deve ocorrer (ex: `TURN_UPDATE`).  
  2. Ele **publica** este evento para todos os nós seguidores via REST (`/notify-player`).  
  3. Cada nó (Seguidor) verifica se o jogador-alvo está conectado a ele.  
  4. Se estiver, o nó **envia (push)** a mensagem pela conexão TCP daquele cliente específico.

Essa abordagem evita a necessidade de um *broker* externo, integrando o broadcast à lógica do Raft e usando o próprio TCP como canal de entrega.

---

### 4. Gerenciamento Distribuído de Estoque

Este é um dos pontos mais críticos do sistema e é resolvido integralmente pelo Raft.

> **Problema:** Como garantir que dois jogadores em servidores diferentes não comprem o *mesmo* pacote único?

**Solução (Consenso do Líder):**

1. O "estoque global" (`packetStock`) é um mapa replicado na FSM de todos os nós.  
2. O Líder lineariza todas as requisições.  
3. A FSM processa uma de cada vez, garantindo exclusividade de item.  
4. O Raft garante que o log é aplicado na mesma ordem em todos os nós — logo, dois jogadores nunca recebem o mesmo item.

---

### 5. Consistência e Justiça do Estado do Jogo

Todo o estado do jogo (saldo, inventários, progresso da partida) é gerenciado pela FSM e protegido pelo Raft.

**Exemplo: Troca de Cartas (`PROPOSE_TRADE`):**

1. O jogador propõe uma troca.  
2. A FSM verifica se a oferta reversa existe.  
3. Se sim, executa a troca de forma **atômica** — sem inconsistências.  
4. Se não, registra a proposta.

**Exemplo: Partida (`PLAY_NOTE_RAFT`):**

1. A FSM valida o turno do jogador.  
2. Atualiza a sequência e o turno.  
3. O Líder é a única fonte da verdade, garantindo consistência forte.

---

### 6. Pareamento em Ambiente Distribuído

O pareamento funciona independentemente de onde os jogadores estão conectados, pois o "lobby" (`salasEmEspera` e `salas`) é um estado replicado pela FSM.

1. Jogador 1 cria uma sala (FSM adiciona a `salasEmEspera`).  
2. Jogador 2 encontra e entra na sala.  
3. A FSM move a sala para `Em_Jogo`.  
4. O Líder notifica ambos os jogadores (um via REST → TCP).

---

### 7. Tolerância a Falhas e Resiliência

**Falha de Seguidor:** O cluster continua operando normalmente; clientes podem se reconectar a outro nó.  
**Falha de Líder:** Um novo Líder é eleito automaticamente.  

**Reconexão de Cliente:**

1. O cliente tenta outro servidor da lista (`ReconnectLoop`).  
2. Envia `RECONNECT_SESSION` com seu login.  
3. O novo nó vincula a sessão ativa ao socket recém-criado.

---

## ⚙️ Como Executar (Docker - Item 9)

O `docker-compose.yml` e o `Makefile` orquestram o ambiente distribuído.

### 1. Construir e Iniciar o Cluster (3 Nós)

```bash
make all
# ou
docker-compose up --build -d
````

### 2. Visualizar os Logs

```bash
docker-compose logs -f
```

### 3. Escalar o Cluster

```bash
docker-compose up --build -d --scale servidor=5
```

### 4. Rodar o Cliente

```bash
go run ./cmd/cliente/cliente.go
```

### 5. Encerrar o Ambiente

```bash
make down
```

---

## 🧪 Como Testar (Testes de Software - Item 8)

Foram desenvolvidos testes de integração automatizados (`cmd/servidor/servidor_test.go`) que validam o sistema em um cenário realista.

**Para rodar os testes:**

```bash
cd cmd/servidor
go test -v -timeout 60s
```

**Resultado esperado:**

```
=== RUN   TestCadastroELogin
=== RUN   TestCadastroELogin/Cadastro
=== RUN   TestCadastroELogin/LoginCorreto
=== RUN   TestCadastroELogin/CadastroRepetido
=== RUN   TestCadastroELogin/LoginSenhaErrada
--- PASS: TestCadastroELogin (0.01s)
    --- PASS: TestCadastroELogin/Cadastro (0.00s)
    --- PASS: TestCadastroELogin/LoginCorreto (0.00s)
    --- PASS: TestCadastroELogin/CadastroRepetido (0.00s)
    --- PASS: TestCadastroELogin/LoginSenhaErrada (0.00s)
=== RUN   TestCompraPacote
=== RUN   TestCompraPacote/CompraComSaldo
=== RUN   TestCompraPacote/ChecarSaldoPosCompra
=== RUN   TestCompraPacote/EsgotarSaldo
--- PASS: TestCompraPacote (0.00s)
    --- PASS: TestCompraPacote/CompraComSaldo (0.00s)
    --- PASS: TestCompraPacote/ChecarSaldoPosCompra (0.00s)
    --- PASS: TestCompraPacote/EsgotarSaldo (0.00s)
=== RUN   TestPartidaSimples
=== RUN   TestPartidaSimples/CriarSala
=== RUN   TestPartidaSimples/EntrarSalaEIniciar
    servidor_test.go:497: Partida de um turno concluída com sucesso.
--- PASS: TestPartidaSimples (0.00s)
    --- PASS: TestPartidaSimples/CriarSala (0.00s)
    --- PASS: TestPartidaSimples/EntrarSalaEIniciar (0.00s)
PASS
ok      pbl_redes/server        1.234s
```

```

---

Quer que eu adicione um **sumário com links internos (Table of Contents)** no início do README também? Isso deixaria a navegação mais prática, especialmente se for entregue no GitHub.
```
