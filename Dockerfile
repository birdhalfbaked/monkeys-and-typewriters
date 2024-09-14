FROM golang:1.23-alpine

COPY . /app

WORKDIR /app

ENTRYPOINT [ "./run.sh"]
