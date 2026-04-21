FROM golang:1.26.2 AS builder

ARG TARGETOS
ARG TARGETARCH
ARG BINARY=apiservice-audit-proxy

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN case "${BINARY}" in \
		apiservice-audit-proxy) target="./cmd/server" ;; \
		mock-audit-webhook) target="./cmd/mock-audit-webhook" ;; \
		*) echo "unsupported BINARY=${BINARY}" >&2; exit 1 ;; \
	esac \
	&& CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -o /out/apiservice-audit-proxy "${target}"

FROM gcr.io/distroless/static:debug

COPY --from=builder /out/apiservice-audit-proxy /apiservice-audit-proxy
USER 65532:65532

ENTRYPOINT ["/apiservice-audit-proxy"]
