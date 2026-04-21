FROM golang:1.26.2 AS builder

ARG TARGETOS
ARG TARGETARCH
ARG BINARY=audit-pass-through-apiserver

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN case "${BINARY}" in \
		audit-pass-through-apiserver) target="./cmd/server" ;; \
		mock-audit-webhook) target="./cmd/mock-audit-webhook" ;; \
		*) echo "unsupported BINARY=${BINARY}" >&2; exit 1 ;; \
	esac \
	&& CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -o /out/audit-pass-through-apiserver "${target}"

FROM gcr.io/distroless/static:debug

COPY --from=builder /out/audit-pass-through-apiserver /audit-pass-through-apiserver
USER 65532:65532

ENTRYPOINT ["/audit-pass-through-apiserver"]
