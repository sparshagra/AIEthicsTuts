# The match runs it with no network, a read-only root (writable /tmp), 1 CPU, 512 MB, and the map at /arena.
FROM python:3.13-slim
RUN pip install --no-cache-dir numpy==2.3.4
WORKDIR /bot
COPY bot.py ffa_brain.py practice_brain.py mechfight.py world.py ./
COPY arena ./arena
ENTRYPOINT ["python3", "-u", "/bot/bot.py"]
