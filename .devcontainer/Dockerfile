FROM golang:1.26.2-bookworm AS ci

ENV DEBIAN_FRONTEND=noninteractive
ENV PATH="/go/bin:/usr/local/go/bin:${PATH}" \
    KUBECTL_VERSION=v1.35.3 \
    KUSTOMIZE_VERSION=5.8.1 \
    KUBEBUILDER_VERSION=4.13.1 \
    GOLANGCI_LINT_VERSION=v2.11.4 \
    HELM_VERSION=v4.1.4 \
    K3D_VERSION=v5.8.3 \
    FLUX_VERSION=2.8.5 \
    TASK_VERSION=v3.49.1

RUN apt-get update \
    && apt-get -y install --no-install-recommends \
    ca-certificates \
    bubblewrap \
    curl \
    git \
    gettext-base \
    jq \
    less \
    acl \
    gnupg \
    rsync \
    sudo \
    vim \
    yq \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
       > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get -y install --no-install-recommends docker-ce-cli \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

RUN test "$(dpkg --print-architecture)" = "amd64" \
    || (echo "This devcontainer currently supports amd64 only." && exit 1)

RUN curl -LO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x kubectl \
    && mv kubectl /usr/local/bin/

RUN curl -Lo kustomize.tar.gz "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_linux_amd64.tar.gz" \
    && tar -zxvf kustomize.tar.gz \
    && mv kustomize /usr/local/bin/ \
    && rm kustomize.tar.gz

RUN curl -L -o /usr/local/bin/kubebuilder "https://github.com/kubernetes-sigs/kubebuilder/releases/download/v${KUBEBUILDER_VERSION}/kubebuilder_linux_amd64" \
    && chmod +x /usr/local/bin/kubebuilder

RUN curl -fsSL https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz \
    | tar -xzO linux-amd64/helm > /usr/local/bin/helm \
    && chmod +x /usr/local/bin/helm

RUN curl -sSfL https://golangci-lint.run/install.sh \
    | sh -s -- -b /usr/local/bin ${GOLANGCI_LINT_VERSION}

RUN curl -fsSL -o /usr/local/bin/k3d "https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}/k3d-linux-amd64" \
    && chmod +x /usr/local/bin/k3d

RUN curl -fsSL "https://github.com/fluxcd/flux2/releases/download/v${FLUX_VERSION}/flux_${FLUX_VERSION}_linux_amd64.tar.gz" \
    | tar -xzO flux > /usr/local/bin/flux \
    && chmod +x /usr/local/bin/flux

RUN curl -fsSL https://github.com/go-task/task/releases/download/${TASK_VERSION}/task_linux_amd64.tar.gz \
    | tar -xzO task > /usr/local/bin/task \
    && chmod +x /usr/local/bin/task

WORKDIR /workspaces

RUN groupadd --gid 2000 godev

RUN chgrp -R godev /go \
    && chmod -R 2775 /go \
    && setfacl -d -m g:godev:rwx /go \
    && setfacl -d -m u::rwx /go \
    && setfacl -d -m o::rx /go

RUN go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 \
    && go install github.com/onsi/ginkgo/v2/ginkgo@v2.28.1 \
    && go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.0.0-20260318145839-6c9615a2a166 \
    && go install golang.org/x/tools/cmd/goimports@v0.43.0

RUN mkdir -p /tmp/golangci-init \
    && cd /tmp/golangci-init \
    && go mod init example.com/init \
    && echo 'package main\n\nfunc main() {}' > main.go \
    && golangci-lint run --timeout=5m || true \
    && cd / \
    && rm -rf /tmp/golangci-init

RUN printf '%s\n' \
    '# Ensure Go and Go-installed tools are available in login shells.' \
    'case ":$PATH:" in' \
    '  *:/usr/local/go/bin:*) ;;' \
    '  *) export PATH="/go/bin:/usr/local/go/bin:$PATH" ;;' \
    'esac' \
    > /etc/profile.d/go-path.sh \
    && chmod 0644 /etc/profile.d/go-path.sh

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

ENV TILT_VERSION=v0.37.1
RUN arch="$(dpkg --print-architecture)" && \
    case "${arch}" in \
        amd64) tilt_arch="x86_64" ;; \
        arm64) tilt_arch="arm64" ;; \
        *) echo "unsupported architecture for Tilt: ${arch}" && exit 1 ;; \
    esac && \
    tilt_version_no_v="${TILT_VERSION#v}" && \
    url="https://github.com/tilt-dev/tilt/releases/download/${TILT_VERSION}/tilt.${tilt_version_no_v}.linux.${tilt_arch}.tar.gz" && \
    echo "Downloading ${url}" && \
    curl -fsSL -o /tmp/tilt.tar.gz "${url}" && \
    tar -xzf /tmp/tilt.tar.gz -C /tmp tilt && \
    install -m 0755 /tmp/tilt /usr/local/bin/tilt && \
    rm -f /tmp/tilt /tmp/tilt.tar.gz

RUN go install github.com/go-delve/delve/cmd/dlv@v1.26.1

RUN go install golang.org/x/tools/gopls@v0.21.1 \
    && go install honnef.co/go/tools/cmd/staticcheck@v0.7.0

RUN groupadd --gid 1000 vscode \
    && useradd --uid 1000 --gid vscode --shell /bin/bash --create-home vscode \
    && echo 'vscode ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers.d/vscode \
    && chmod 0440 /etc/sudoers.d/vscode \
    && usermod -aG godev vscode

RUN groupadd -f docker && usermod -aG docker vscode

RUN chown -R vscode:vscode /workspaces \
    && chmod -R 755 /workspaces

USER vscode

ENV DEBIAN_FRONTEND=dialog

CMD ["/bin/bash"]
