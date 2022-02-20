A simple Go script that provides an endpoint to run arbitrary containers, typically used on a sandbox server.

## Quick start

```bash
docker pull ghcr.io/micromaomao/procschd:latest
echo -n MY_TOKEN > auth-token-file
docker run --detach -v /var/run/docker.sock:/var/run/docker.sock -v $(realpath auth-token-file):/auth-token:ro -u 0:0 -p 3000:3000 ghcr.io/micromaomao/procschd --listen "tcp:[::]:3000" --auth-token-file /auth-token
```
