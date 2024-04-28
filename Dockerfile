FROM golang as build
WORKDIR /build

COPY go.* ./
RUN go mod download

COPY . ./
RUN go build -o server .

FROM ubuntu

RUN mkdir -p /app
COPY --from=build /build/server /app/
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENV TZ=UTC
ENV HOST="0.0.0.0"
ENV PORT="8080"
ENTRYPOINT ["/app/server"]
