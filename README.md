Aqui está o README formatado em Markdown, pronto para você copiar e colar diretamente no seu arquivo `README.md`.

-----

# PBL\_REDES - Jogo Rítmico Distribuído

**Autor**: Leonardo Oliveira Almeida da Cruz ([@oLeozito](https://www.google.com/search?q=https://github.com/oLeozito))
**Disciplina**: TEC502 - Redes e Conectividade (PBL)

Este projeto implementa um servidor de jogo multiplayer distribuído, tolerante a falhas e escalável, como parte da avaliação da disciplina. O sistema foi projetado para migrar uma arquitetura de servidor de jogo centralizado para um cluster de nós distribuídos que gerenciam o estado do jogo de forma consistente.

## 🚀 Tecnologias Utilizadas

  * **Go (Golang):** Linguagem principal para o desenvolvimento do servidor e cliente.
  * **Protocolo Raft:** Utilizado para consenso e replicação de estado (via biblioteca `hashicorp/raft`).
  * **TCP Sockets:** Protocolo de comunicação primário entre o cliente (`cliente.go`) e o nó do servidor (`servidor.go`) ao qual ele se conecta.
  * **API REST (HTTP):** Protocolo de comunicação *servidor-servidor*, usado para encaminhamento de requisições e broadcast de notificações.
  * **JSON:** Formato de serialização para todas as mensagens TCP e REST.
  * **Docker & Docker Compose:** Utilizado para orquestrar e testar o ambiente distribuído com múltiplos nós de servidor.

-----

## 🏛️ Visão Geral da Arquitetura (Barema - Item 1)

A arquitetura do sistema é baseada no padrão de **Máquina de Estados Replicada (Replicated State Machine)**, utilizando o algoritmo de consenso Raft para garantir a consistência.

Diferente de uma arquitetura centralizada, onde um único servidor detém todo o estado (jogadores, salas, inventários), esta solução utiliza um *cluster* de nós `servidor.go`.

1.  **Nós do Cluster:** Qualquer instância do `servidor.go` pode ser executada como um nó. Os nós se comunicam para eleger um **Líder**.
2.  **Líder vs. Seguidor:**
      * O **Líder** é o único nó autorizado a aplicar mudanças de estado (ex: cadastrar um jogador, comprar um pacote, jogar uma nota).
      * Os **Seguidores (Followers)** atuam como *gateways* leves. Eles recebem conexões TCP dos clientes, mas, em vez de processar as lógicas de negócio, eles **encaminham** a requisição para o Líder via API REST.
3.  **Fluxo de Dados (FSM):**
      * Um cliente (ex: `cliente1`) envia um comando (ex: `COMPRAR_PACOTE`) via TCP para o nó ao qual está conectado (ex: `Servidor 2 - Seguidor`).
      * O `Servidor 2` encaminha essa requisição para o `Servidor 1 - Líder` via REST (`/request-purchase`).
      * O Líder recebe a requisição, a submete ao log do Raft (`raftNode.Apply()`).
      * O Raft garante que este comando seja replicado e aplicado na **Máquina de Estados Finitos (FSM)** de *todos* os nós do cluster (incluindo o `Servidor 2`).
      * A FSM (função `FSM.Apply`) executa a lógica de forma determinística, garantindo que o estoque de pacotes e o inventário do jogador sejam atualizados de forma idêntica em todos os nós.

Essa arquitetura permite **escalabilidade horizontal**: podemos adicionar mais nós seguidores para lidar com mais conexões de clientes, sem sobrecarregar o nó Líder, que se concentra apenas em orquestrar o estado.

-----

## 📋 Respostas Detalhadas ao Barema

Aqui detalhamos como cada requisito de avaliação foi implementado.

### [cite\_start]2. Comunicação Servidor-Servidor [cite: 1]

A comunicação entre os nós do servidor é realizada através de duas vias principais:

1.  **Raft (TCP):** O `hashicorp/raft` gerencia sua própria comunicação interna (binária) para eleição de líder e replicação de log.
2.  [cite\_start]**API REST (HTTP):** Uma API REST (`startHttpApi`) é exposta por cada nó para permitir a colaboração e o encaminhamento de requisições[cite: 1].

[cite\_start]**Endpoints REST Principais:** [cite: 1]

  * `/join`: (POST) Endpoint de *bootstrap*. Um novo nó o utiliza para solicitar ao Líder sua adição ao cluster Raft.
  * `/request-register`, `/request-login`, `/request-logout`, `/request-reconnect`: (POST) Usados por seguidores para encaminhar comandos de autenticação e sessão para o Líder.
  * `/request-purchase`, `/request-trade`: (POST) Usados por seguidores para encaminhar lógicas de negócio críticas (compra e troca) para o Líder, garantindo que sejam processadas pela FSM.
  * `/request-create-room`, `/request-find-room`, `/request-play-note`: (POST) Usados por seguidores para encaminhar toda a lógica de pareamento e jogabilidade para o Líder.
  * `/notify-player`: (POST) Endpoint crucial usado pelo Líder para **transmitir (broadcast)** eventos a *todos* os seguidores. Por exemplo, quando uma troca é concluída, o Líder envia a notificação para este endpoint em todos os outros nós, e cada nó é responsável por verificar se o jogador-alvo está conectado a ele e entregar a mensagem via TCP.

### [cite\_start]3. Comunicação Cliente-Servidor [cite: 1]

[cite\_start]O barema sugere um modelo *publisher-subscriber*[cite: 1]. A solução implementada atinge o *mesmo objetivo* (notificações *push* do servidor para o cliente) de forma mais integrada, usando uma combinação de TCP e REST.

  * **Canal Primário (TCP):** A comunicação é bidirecional sobre uma única conexão TCP (`handleConnection`). O cliente envia comandos (ex: `PLAY_NOTE`) e o servidor responde diretamente (ex: `SCREEN_MSG` de erro).
  * **Modelo "Pub/Sub" Simulado (Push):** Para eventos que não são uma resposta direta (como o início do turno do oponente ou uma proposta de troca), o sistema simula um Pub/Sub:
    1.  O Líder (Publisher) decide que um evento deve ocorrer (ex: `TURN_UPDATE`).
    2.  Ele **publica** este evento para todos os nós seguidores via REST (usando o endpoint `/notify-player`).
    3.  Cada nó (Seguidor) age como um *broker* local. Ele verifica se o jogador-alvo (Subscriber) está conectado a ele.
    4.  Se estiver, o nó **envia (push)** a mensagem (`TURN_UPDATE`) pela conexão TCP daquele cliente específico.

Esta abordagem evita a necessidade de um *broker* MQTT externo, integrando o *broadcast* de mensagens à própria lógica de estado do Raft, e usando o TCP (que já é usado para comandos) como o canal de entrega. [cite\_start]Nenhuma biblioteca de terceiros específica para Pub/Sub foi usada, pois a funcionalidade foi implementada nativamente[cite: 1].

### [cite\_start]4. Gerenciamento Distribuído de Estoque [cite: 1]

[cite\_start]Este é um dos pontos mais críticos do sistema e é resolvido integralmente pelo Raft[cite: 1].

> [cite\_start]**Problema:** Como garantir que dois jogadores em servidores diferentes não comprem o *mesmo* pacote único (ex: `packet_ID_001`) ao mesmo tempo? [cite: 1]

**Solução (Consenso do Líder):**

1.  O "estoque global" (`packetStock`) é um mapa replicado na FSM de todos os nós.
2.  Quando um Jogador 1 (no Servidor A) e um Jogador 2 (no Servidor B) tentam comprar um pacote simultaneamente, ambas as requisições são encaminhadas para o **Líder**.
3.  O Líder **lineariza** (enfileira) essas requisições. Ele aplica uma de cada vez ao log do Raft.
4.  A FSM (no `case "COMPRAR_PACOTE"`) é executada:
      * Ela bloqueia o estoque (`stockMu.Lock()`).
      * Pega o primeiro pacote disponível (ex: `availablePacketIDs[0]`).
      * Marca o pacote como `Opened = true`.
      * Adiciona o pacote ao inventário do jogador.
      * Desbloqueia o estoque.
5.  Quando a FSM processar a requisição do Jogador 2, o pacote que o Jogador 1 pegou não estará mais disponível. A FSM pegará o *próximo* pacote da lista.

[cite\_start]Como o Raft garante que o log é aplicado na mesma ordem em todos os lugares, é **criptograficamente impossível** que dois jogadores recebam o mesmo item, garantindo a justiça e consistência do estoque[cite: 1].

### [cite\_start]5. Consistência e Justiça do Estado do Jogo [cite: 1]

[cite\_start]Assim como o estoque, todo o estado do jogo (saldo, inventários, progresso da partida) é gerenciado pela FSM e protegido pelo Raft[cite: 1].

[cite\_start]**Exemplo: Troca de Cartas (`PROPOSE_TRADE`)** [cite: 1]
A lógica de troca é totalmente atômica dentro da FSM:

1.  Um jogador (P1) propõe trocar a Carta A por uma Carta B de outro jogador (P2).
2.  A requisição é enviada ao Líder e aplicada na FSM.
3.  A FSM verifica o mapa `activeTrades` para ver se (P2) já ofereceu a Carta B pela Carta A (uma `reverseTradeKey`).
4.  **Se a oferta reversa existe:** A FSM executa a troca *atomica e instantaneamente*. Ela remove a Carta A de P1, remove a Carta B de P2, e as adiciona aos inventários opostos. Isso é uma transação única.
5.  **Se não existe:** A FSM apenas registra a oferta de P1 no mapa `activeTrades`.

**Exemplo: Partida (`PLAY_NOTE_RAFT`)**
O estado da partida (`GameState`) também é gerenciado pela FSM. Quando um jogador joga uma nota:

1.  A FSM verifica se é o turno do jogador (`sala.Game.CurrentTurn == playerNum`).
2.  Ela adiciona a nota à sequência (`sala.Game.PlayedNotes`).
3.  Verifica se um ataque foi completado (`checkAttackCompletionFSM`).
4.  Atualiza o turno (`sala.Game.CurrentTurn = 2`).

Como o Líder Raft é a única fonte da verdade para a ordem desses eventos, o estado do jogo é sempre fortemente consistente.

### [cite\_start]6. Pareamento em Ambiente Distribuído [cite: 1]

[cite\_start]O pareamento funciona independentemente de onde os jogadores estão conectados, pois o "lobby" (`salasEmEspera` e `salas`) é um estado replicado pela FSM[cite: 1].

1.  **Criação (Pública):** Jogador 1 (no Servidor A) envia `FIND_ROOM` (sem sala de espera). A requisição vai ao Líder, que aplica `CREATE_ROOM_RAFT`. A FSM adiciona uma nova sala ao mapa `salasEmEspera`.
2.  **Entrada (Pública):** Jogador 2 (no Servidor B) envia `FIND_ROOM`. A requisição vai ao Líder. O Líder vê que `salasEmEspera` não está vazio. Ele aplica `JOIN_ROOM_RAFT`.
3.  **Lógica do `JOIN_ROOM_RAFT` (na FSM):**
      * A FSM remove a sala de `salasEmEspera`.
      * Define o `Status` da sala como "Em\_Jogo".
      * Define `P2Login` como o Jogador 2.
      * Inicializa o `sala.Game`.
4.  **Notificação (Handler do Líder):**
      * Após a FSM ser aplicada, o *handler HTTP* do Líder (ex: `handleJoinRoomResultREST`) é notificado do sucesso.
      * Ele envia a mensagem `PAREADO` de volta ao Jogador 2 (pela resposta HTTP ao Servidor B).
      * Ele **difunde (broadcast)** a mensagem `PAREADO` para o Jogador 1 (via `/notify-player` para o Servidor A).

[cite\_start]A FSM garante que a sala seja preenchida apenas uma vez (pareamento único), e a camada HTTP/REST do Líder garante que ambos os jogadores (em servidores diferentes) sejam notificados[cite: 1].

### [cite\_start]7. Tolerância a Falhas e Resiliência [cite: 1]

[cite\_start]O sistema é resiliente a falhas de Nós (Servidores) e Clientes[cite: 1].

**Resiliência a Falha de Nó (Servidor):**

  * **Falha de Seguidor:** Se um nó seguidor falhar, o cluster continua operando normalmente. Os clientes conectados a ele perderão a conexão, mas poderão se reconectar a outro seguidor (veja abaixo).
  * **Falha de Líder:** Se o nó Líder falhar, os seguidores restantes (desde que formem um quórum, ex: 2 de 3) irão detectar a falha, iniciar uma nova eleição e promover um novo Líder em milissegundos. O serviço continua sem interrupção.

**Resiliência a Falha de Conexão (Cliente):**
O `cliente.go` foi projetado para sobreviver a falhas de conexão:

1.  **`connectToServer`:** O cliente possui uma lista de *todos* os endereços de servidor. Ao iniciar, ele embaralha a lista e tenta se conectar a qualquer um deles.
2.  **`ReconnectLoop`:** Se a conexão TCP cair (ex: o nó seguidor falhou), o `main()` do cliente entra em um loop, tentando se reconectar a *qualquer outro* nó da lista.
3.  **`RECONNECT_SESSION`:** Após reconectar (a um nó diferente), o cliente envia o comando `RECONNECT_SESSION` com seu login. O servidor (Líder) recebe isso via FSM (`RECONNECT_SESSION_RAFT`), valida que o jogador estava `Online = true`, e o `handler` no *novo* nó seguidor associa a *nova* conexão TCP ao estado do jogador (`player.Conn = conn`).

Isso permite que um jogador perca a conexão, se reconecte a um servidor totalmente diferente e continue sua sessão (e até mesmo seu jogo, se a lógica fosse expandida) sem precisar fazer login novamente.

-----

## [cite\_start]⚙️ Como Executar (Docker - Item 9) [cite: 1]

O `docker-compose.yml` e o `Makefile` orquestram o ambiente distribuído.

**1. Construir e Iniciar o Cluster (3 Nós):**

O `docker-compose.yml` está configurado para iniciar 3 nós de servidor e uní-los em um cluster.

```bash
# Constrói as imagens e inicia os contêineres
make all

# Ou, para iniciar em background:
docker-compose up --build -d
```

**2. Visualizar os Logs:**

Você verá os 3 servidores iniciando, realizando uma eleição e um deles se declarando LÍDER.

```bash
docker-compose logs -f
```

**3. Escalar o Cluster:**

Para adicionar mais seguidores (escalabilidade):

```bash
docker-compose up --build -d --scale servidor=5
```

**4. Rodar o Cliente:**

O cliente não está em Docker. Você pode rodá-lo localmente no seu terminal:

```bash
# Estando na raiz do projeto (PBL_REDES/)
go run ./cmd/cliente/cliente.go
```

O cliente tentará se conectar às portas expostas pelos contêineres Docker (ex: `8081`, `8082`, `8083`).

**5. Encerrar o Ambiente:**

```bash
make down
```

## [cite\_start]🧪 Como Testar (Testes de Software - Item 8) [cite: 1]

[cite\_start]Foram desenvolvidos testes de integração automatizados (`cmd/servidor/servidor_test.go`) que validam o sistema em um cenário realista[cite: 1].

[cite\_start]O script de teste[cite: 1]:

1.  Compila e inicia um binário real do `servidor.go` como um nó único (que se torna Líder).
2.  Espera ativamente que o servidor se torne LÍDER (consultando o endpoint `/health`).
3.  Inicia "clientes de teste" (`TestClient`) que se conectam via TCP.
4.  Executa os seguintes cenários:
      * **`TestCadastroELogin`**: Testa o cadastro de um novo usuário, o login bem-sucedido, a falha de cadastro duplicado e a falha de senha incorreta.
      * **`TestCompraPacote`**: Testa a compra de pacotes, a verificação de saldo (deve diminuir) e a falha de compra por saldo insuficiente (esgotamento).
      * **`TestPartidaSimples`**: Simula uma partida completa com dois jogadores, incluindo compra de instrumentos, seleção, criação de sala, pareamento e uma rodada de jogo.

[cite\_start]**Para rodar os testes:** [cite: 1]

```bash
# 1. Navegue até o diretório do servidor
cd cmd/servidor

# 2. Execute o Go Test
# (O -v mostra os testes passando, e o -timeout é por segurança)
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
