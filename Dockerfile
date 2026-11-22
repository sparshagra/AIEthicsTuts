# AIEthicsTuts - Mech Royale bot by Sparsh (Team 4141). MIT licensed, see LICENSE and NOTICE.
# Plain JavaScript on Node, no dependencies. The match runs it with no network, a read-only root (writable /tmp),
# 1 CPU, 512 MB, uid 65534, and the map at /arena.
FROM node:22.12.0-bookworm-slim@sha256:35531c52ce27b6575d69755c73e65d4468dba93a25644eed56dc12879cae9213
WORKDIR /bot
COPY src ./src
ENV NODE_ENV=production
USER 65534:65534
ENTRYPOINT ["node", "--max-old-space-size=320", "--expose-gc", "/bot/src/main.mjs"]
