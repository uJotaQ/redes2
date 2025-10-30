# Define que os comandos não geram arquivos com esses nomes
.PHONY: all up down build clean cliente \
        servidor1 servidor2 servidor3 \
        parar1 parar2 parar3 \
        logs1 logs2 logs3

# --- Comandos de Controle Global ---

# Sobe todo o ambiente (3 servidores + 1 broker)
up:
	@echo "Subindo broker e servidor1..."
	docker compose up --build -d servidor1
	@echo "Aguardando 0.5 segundos para o servidor1 se estabilizar..."
	@sleep 0.1
	@echo "Subindo servidor2 ..."
	docker compose up --build -d servidor2
	@echo "Aguardando 0.5 segundos..."
	@sleep 0.1
	@echo "Subindo servidor3 ..."
	docker compose up --build -d servidor3
	@echo "Cluster completo!"

# Para e remove todos os contêineres
down:
	docker compose down --remove-orphans

# Apenas constrói a imagem, sem iniciar
build:
	docker compose build


# # Limpa os dados dos servidores (logs do raft, etc.) E corrige a permissão
# clean:
# 	@echo "Limpando diretórios de dados com permissão de administrador..."
#     # 1. Remove os dados antigos
# 	rm -rf ./data/servidor1 ./data/servidor2 ./data/servidor3
#     # 2. Recria os diretórios vazios (necessário para o próximo passo)
# 	mkdir -p ./data/servidor1 ./data/servidor2 ./data/servidor3
#     # 3. GARANTE que você é o dono dos diretórios vazios recém-criados
#     sudo chown -R $$(id -u):$$(id -g) ./data
# 	@echo "Limpeza concluída e permissões corrigidas para o usuário atual."

# --- Comandos de Controle Individual ---

# Inicia o servidor 1 (e o broker, caso não esteja rodando)
servidor1:
	docker compose up --build -d servidor1

# Inicia o servidor 2
servidor2:
	docker compose up --build -d servidor2

# Inicia o servidor 3
servidor3:
	docker compose up --build -d servidor3

# Para o servidor 1
parar1:
	docker compose stop servidor1
	docker compose rm -f servidor1

# Para o servidor 2
parar2:
	docker compose stop servidor2
	docker compose rm -f servidor2

# Para o servidor 3
parar3:
	docker compose stop servidor3
	docker compose rm -f servidor3


# --- Comandos de Visualização e Cliente ---

# Mostra os logs do servidor 1
logs1:
	docker compose logs -f servidor1

# Mostra os logs do servidor 2
logs2:
	docker compose logs -f servidor2

# Mostra os logs do servidor 3
logs3:
	docker compose logs -f servidor3

# Roda o cliente localmente
cliente:
	go run cliente.go