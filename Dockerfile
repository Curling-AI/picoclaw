# ============================================================
# Stage 1: Build the picoclaw binary
# ============================================================
FROM golang:1.26.0-alpine AS builder

RUN apk add --no-cache git make

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN make build

# ============================================================
# Stage 2: Minimal runtime image
# ============================================================
FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata curl \
    nodejs npm git chromium github-cli

# Install agent-browser globally (uses system Chromium via AGENT_BROWSER_EXECUTABLE_PATH)
ENV CHROME_PATH=/usr/bin/chromium-browser
ENV AGENT_BROWSER_EXECUTABLE_PATH=/usr/bin/chromium-browser
ENV CHROMIUM_FLAGS="--no-sandbox"
RUN npm install -g agent-browser @anthropic-ai/claude-code

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q --spider http://localhost:18790/health || exit 1

# Copy binary
COPY --from=builder /src/build/picoclaw /usr/local/bin/picoclaw

# Create non-root user and group
RUN addgroup -g 1000 picoclaw && \
    adduser -D -u 1000 -G picoclaw picoclaw

# Switch to non-root user
USER picoclaw

# Run onboard to create initial directories and config
RUN /usr/local/bin/picoclaw onboard

# Pre-install skills SKILL.md files into PicoClaw global skills dir
RUN cd /home/picoclaw/.picoclaw && \
    for skill in \
      "https://github.com/vercel-labs/skills|find-skills" \
      "https://github.com/vercel-labs/agent-browser|agent-browser" \
      "https://github.com/github/awesome-copilot/gh-cli|gh-cli" \
      "https://github.com/anthropics/skills|pdf" \
      "https://github.com/anthropics/skills|docx" \
      "https://github.com/anthropics/skills|pptx" \
      "https://github.com/anthropics/skills|xlsx" \
      "https://github.com/anthropics/claude-code|Skill Development"; do \
      npx skills add "${skill%|*}" --skill "${skill#*|}" -y -a '*'; \
    done

# Create workspace bin directory with symlinks to all tools
# so the exec tool's restrictToWorkspace guard doesn't block them
RUN mkdir -p /home/picoclaw/.picoclaw/workspace/bin && \
    ln -s /usr/bin/gh /home/picoclaw/.picoclaw/workspace/bin/gh && \
    ln -s /usr/bin/git /home/picoclaw/.picoclaw/workspace/bin/git && \
    ln -s /usr/bin/node /home/picoclaw/.picoclaw/workspace/bin/node && \
    ln -s /usr/bin/npm /home/picoclaw/.picoclaw/workspace/bin/npm && \
    ln -s /usr/bin/npx /home/picoclaw/.picoclaw/workspace/bin/npx && \
    ln -s /usr/bin/chromium-browser /home/picoclaw/.picoclaw/workspace/bin/chromium-browser && \
    ln -s "$(which claude)" /home/picoclaw/.picoclaw/workspace/bin/claude && \
    ln -s "$(which agent-browser)" /home/picoclaw/.picoclaw/workspace/bin/agent-browser

ENV PATH="/home/picoclaw/.picoclaw/workspace/bin:$PATH"

ENTRYPOINT ["picoclaw"]
CMD ["gateway"]
