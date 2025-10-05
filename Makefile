# Define que os comandos não geram arquivos com esses nomes
.PHONY: servidor cliente parar build

# Comando principal para subir o servidor e o broker em primeiro plano
# O --build garante que qualquer alteração no código seja compilada
servidor:
	docker compose up --build

# Roda o cliente localmente (ainda sem Docker)
cliente:
	go run cliente.go

# Para e remove os contêineres do servidor e do broker
parar:
	docker compose down --remove-orphans

# Apenas constrói a imagem do servidor, sem iniciar
build:
	docker compose build