# Gitea Agent - Docker

On your runner server:

```bash
docker build -t gitea-agent:local .
```

In `app.ini` :

```
CONTAINER_IMAGE = gitea-agent:local
```
