# Configuration

## Global Environment Variables

Every service reads the same environment variables (set in `docker-compose.yml` or `.env`).

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8700` | Port the API listens on |
| `DATA_DIR` | `./data` | Directory the book store keeps its files in |
| `STORE_URL` | `file://./data/books.db` | Where the book store lives |
| `LOG_LEVEL` | `info` | Log level |

## Search

| Variable | Default | Description |
|---|---|---|
| `SEARCH_ENABLED` | `true` | Serve `/search` |
| `SEARCH_INDEX` | `books` | Index the search box queries |
| `SEARCH_PAGE_SIZE` | `20` | Results per page |

## Covers

| Variable | Default | Description |
|---|---|---|
| `COVER_SIZES` | `s,m,l` | Cover sizes the covers worker renders |

## Cleanup

| Variable | Default | Description |
|---|---|---|
| `RETENTION_DAYS` | `30` | Days a deleted book stays recoverable |
