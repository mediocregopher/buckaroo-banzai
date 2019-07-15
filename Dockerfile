FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /
RUN wget https://raw.githubusercontent.com/eficode/wait-for/master/wait-for
RUN chmod 755 wait-for
COPY buckaroo-banzai /
CMD ["/buckaroo-banzai"]
