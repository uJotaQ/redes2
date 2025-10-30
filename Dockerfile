# Usar uma imagem base oficial do Go. A versão 1.18 corresponde ao seu go.mod
FROM golang:1.24-alpine

# Definir o diretório de trabalho dentro do contêiner
WORKDIR /app

# Copiar os arquivos de módulo e baixar as dependências primeiro
COPY go.mod ./
RUN go mod download

# Copiar todo o resto do código-fonte
COPY . .

# Compilar APENAS o servidor
# O -o /server define o nome do executável de saída
RUN go build -o /server servidor.go

# Expor a porta que o servidor usa, para documentação
EXPOSE 8080

# O comando para executar quando o contêiner iniciar
CMD ["/server"]