# 🃏 Jogo de Cartas Multiplayer - PBL de Redes 2

![Go](https://img.shields.io/badge/Go-00ADD8?style=for-the-badge&logo=go&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-2496ED?style=for-the-badge&logo=docker&logoColor=white)
![Makefile](https://img.shields.io/badge/GNU%20Make-427819?style=for-the-badge&logo=gnu-make&logoColor=white)

Repositório dedicado ao desenvolvimento do segundo Projeto de Aprendizagem Baseada em Problemas (PBL) da disciplina de Redes de Computadores.

## 🎓 Contexto do Projeto

Este projeto foi desenvolvido como parte da avaliação da disciplina de **Redes de Computadores 2**. O objetivo principal era aplicar os conceitos de comunicação em rede (como sockets, protocolos e arquitetura cliente-servidor) na prática, através da criação de um jogo de cartas multiplayer funcional.

## 📝 Descrição do Jogo

O projeto implementa um jogo de cartas multiplayer .

Ele utiliza uma arquitetura cliente-servidor onde um processo (`servidor.go`) atua como o *host* central do jogo, gerenciando o estado, as regras e a conexão dos jogadores, enquanto múltiplos processos (`cliente.go`) se conectam a ele para participar da partida.

## 🏗️ Arquitetura e Componentes

O sistema é dividido principalmente em três componentes:

* **Servidor (`servidor.go`):**
    * Responsável por aguardar e aceitar conexões de novos jogadores.
    * Gerencia o *lobby* e o início da partida.
    * Controla o estado do jogo (turnos, pontuação, cartas na mesa).
    * Recebe as jogadas dos clientes, valida-as e atualiza o estado para todos os outros jogadores.

* **Cliente (`cliente.go`):**
    * Responsável por se conectar ao servidor.
    * Envia as ações do jogador (ex: jogar uma carta).
    * Recebe as atualizações de estado do servidor e as exibe para o jogador (provavelmente via terminal).

* **Protocolo (`protocolo/`):**
    * Define o formato das mensagens trocadas entre o cliente e o servidor, garantindo que ambos "falem a mesma língua".
    * ## 🛠️ Tecnologias Utilizadas

* **Linguagem Principal:** [Go (Golang)](https://go.dev/)
* **Comunicação em Rede:** Sockets TCP (biblioteca `net` padrão do Go).
* **Containerização:** [Docker](https://www.docker.com/) e [Docker Compose](https://docs.docker.com/compose/) para criar um ambiente de execução padronizado e facilitar os testes.
* **Automação de Build:** [Makefile](https://www.gnu.org/software/make/) para simplificar os comandos de build, execução e limpeza.

## 🚀 Como Executar o Projeto

Graças ao Docker, o projeto pode ser executado facilmente sem a necessidade de instalar o Go manualmente na máquina.

**Pré-requisitos:**
* [Docker](https://www.docker.com/get-started)
* [Docker Compose](https://docs.docker.com/compose/install/)

### 1. Usando Docker Compose (Recomendado)

O `docker-compose.yml` está configurado para construir as imagens e iniciar os contêineres do servidor e (opcionalmente) dos clientes.

1.  Clone o repositório:
    ```bash
    git clone [https://github.com/uJotaQ/redes2.git](https://github.com/uJotaQ/redes2.git)
    cd redes2
    ```

2.  Suba os serviços:
    ```bash
    docker-compose up --build
    ```
    3.  Para rodar clientes adicionais, abra novos terminais e execute:
    ```bash
    docker-compose run cliente
    ```

### 2. Usando Makefile (Alternativa)

O `Makefile` provê comandos de atalho para as operações mais comuns.

```bash
# Para construir as imagens Docker
make build

# Para iniciar os serviços (definido no docker-compose)
make up

# Para derrubar os serviços
make down

# Para limpar imagens e contêineres
make clean
