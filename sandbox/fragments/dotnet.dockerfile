ARG DOTNET_SDK_VERSION=10.0.401

USER root

# ── .NET SDK ─────────────────────────────────────────────────────
RUN apt-get update && apt-get install -y --no-install-recommends libicu74 \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://dot.net/v1/dotnet-install.sh -o /tmp/dotnet-install.sh \
    && chmod +x /tmp/dotnet-install.sh \
    && /tmp/dotnet-install.sh --version "${DOTNET_SDK_VERSION}" --install-dir /usr/share/dotnet \
    && ln -s /usr/share/dotnet/dotnet /usr/local/bin/dotnet \
    && rm /tmp/dotnet-install.sh
ENV DOTNET_ROOT=/usr/share/dotnet
ENV DOTNET_CLI_TELEMETRY_OPTOUT=1
ENV DOTNET_NOLOGO=1

USER dev
