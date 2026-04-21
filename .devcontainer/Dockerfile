FROM golang:1.26.2-bookworm AS ci

ENV DEBIAN_FRONTEND=noninteractive
ENV HELM_VERSION=v4.1.4 \
    GOLANGCI_LINT_VERSION=v2.11.4 \
    TASK_VERSION=v3.49.1

RUN apt-get update \
    && apt-get -y install --no-install-recommends \
    ca-certificates \
    curl \
    git \
    jq \
    sudo \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

RUN test "$(dpkg --print-architecture)" = "amd64" \
    || (echo "This devcontainer currently supports amd64 only." && exit 1)

RUN curl -fsSL https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz \
    | tar -xzO linux-amd64/helm > /usr/local/bin/helm \
    && chmod +x /usr/local/bin/helm

RUN curl -sSfL https://golangci-lint.run/install.sh \
    | sh -s -- -b /usr/local/bin ${GOLANGCI_LINT_VERSION}

RUN curl -fsSL https://github.com/go-task/task/releases/download/${TASK_VERSION}/task_linux_amd64.tar.gz \
    | tar -xzO task > /usr/local/bin/task \
    && chmod +x /usr/local/bin/task

WORKDIR /workspaces

COPY go.mod go.sum ./
RUN go mod download

ENV DEBIAN_FRONTEND=dialog

CMD ["/bin/bash"]

FROM ci AS dev

USER root

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get -y install --no-install-recommends bash-completion \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

RUN printf '%s\n' \
    '' \
    '# Enable bash completion and Task completions in interactive shells.' \
    'if ! shopt -oq posix; then' \
    '  if [ -f /usr/share/bash-completion/bash_completion ]; then' \
    '    . /usr/share/bash-completion/bash_completion' \
    '  elif [ -f /etc/bash_completion ]; then' \
    '    . /etc/bash_completion' \
    '  fi' \
    '  if command -v task >/dev/null 2>&1 && ! complete -p task >/dev/null 2>&1; then' \
    '    eval "$(task --completion bash)"' \
    '  fi' \
    'fi' \
    >> /etc/bash.bashrc

RUN groupadd --gid 1000 vscode \
    && useradd --uid 1000 --gid vscode --shell /bin/bash --create-home vscode \
    && echo 'vscode ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers.d/vscode \
    && chmod 0440 /etc/sudoers.d/vscode

RUN chown -R vscode:vscode /workspaces \
    && chmod -R 755 /workspaces

USER vscode

ENV DEBIAN_FRONTEND=dialog

CMD ["/bin/bash"]
