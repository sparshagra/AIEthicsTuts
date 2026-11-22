# AIEthicsTuts

A `mechfight/1` bot for the Builders Buildathon (Mech Royale track), Team 4141.

The pilot is a compiled Go bot (`cmd/mechbot`, `internal/`) built on Apex-Go (MIT, see `LICENSE-apex`).

```sh
docker build --platform linux/amd64 -t bot .
python3 check/check.py --map-dir /arena -- docker run --rm -i --network none --cpus 1 --memory 512m \
  --read-only --tmpfs /tmp:rw,size=64m --user 65534:65534 -v "$PWD/arena:/arena:ro" bot
```
