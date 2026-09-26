FROM nvidia/cuda:12.1.1-devel-ubuntu22.04

# Configurações do sistema
ENV DEBIAN_FRONTEND=noninteractive

# Instala ferramentas básicas e SSH
RUN apt-get update && apt-get install -y \
    curl \
    openssh-server \
    git \
    sudo \
    && rm -rf /var/lib/apt/lists/*

# Configura SSH para o Vast.ai (Vast injeta as chaves em /root/.ssh/authorized_keys)
RUN mkdir -p /var/run/sshd && \
    sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin yes/' /etc/ssh/sshd_config

# Instala o Ollama oficial
RUN curl -fsSL https://ollama.ai/install.sh | sh

# Expõe as portas do SSH (22) e Ollama (11434)
EXPOSE 22 11434

# Inicia o SSH Daemon
CMD ["/usr/sbin/sshd", "-D"]
