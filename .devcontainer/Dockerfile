FROM golang:1.26.2-bookworm

ENV DEBIAN_FRONTEND=noninteractive
ENV HELM_VERSION=v4.1.4
ENV GOLANGCI_LINT_VERSION=v2.11.4
ENV TASK_VERSION=v3.49.1

RUN apt-get update \
    && apt-get -y install --no-install-recommends \
    bash-completion \
    ca-certificates \
    curl \
    git \
    jq \
    sudo \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz \
    | tar -xzO linux-amd64/helm > /usr/local/bin/helm \
    && chmod +x /usr/local/bin/helm

RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
    | sh -s -- -b /usr/local/bin ${GOLANGCI_LINT_VERSION}

RUN curl -fsSL https://github.com/go-task/task/releases/download/${TASK_VERSION}/task_linux_amd64.tar.gz \
    | tar -xzO task > /usr/local/bin/task \
    && chmod +x /usr/local/bin/task

RUN go install golang.org/x/tools/gopls@v0.21.1

ENV DEBIAN_FRONTEND=dialog
WORKDIR /workspaces
