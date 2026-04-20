FROM golang:1.26.2 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -o /out/audit-pass-through-apiserver ./cmd/server

FROM gcr.io/distroless/static:debug

COPY --from=builder /out/audit-pass-through-apiserver /audit-pass-through-apiserver
USER 65532:65532

ENTRYPOINT ["/audit-pass-through-apiserver"]
