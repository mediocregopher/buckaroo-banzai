FROM golang:1.12 AS builder
WORKDIR /app
COPY . .
RUN wget https://raw.githubusercontent.com/eficode/wait-for/master/wait-for
RUN chmod 755 wait-for
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /
COPY --from=builder /app/wait-for /
COPY --from=builder /app/buckaroo-banzai /
CMD ["/buckaroo-banzai"]
