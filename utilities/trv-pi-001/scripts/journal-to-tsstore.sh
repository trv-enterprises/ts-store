#!/bin/bash
# Stream journalctl output to tsstore with retry/backoff

API_KEY="tsstore_467d0c08-1240-440d-8f65-035762581adf"
TSSTORE_URL="http://localhost:21080"
STORE="journal-logs"
HEALTH_URL="$TSSTORE_URL/health"

MAX_BACKOFF=60
backoff=1

echo "Journal log streamer for tsstore"
echo "Store: $STORE"
echo "URL: $TSSTORE_URL"

while true; do
    # Wait for tsstore to be reachable
    while true; do
        if curl -sf "$HEALTH_URL" > /dev/null 2>&1; then
            echo "tsstore is reachable, starting stream..."
            backoff=1
            break
        fi
        echo "tsstore not reachable, retrying in ${backoff}s..."
        sleep "$backoff"
        backoff=$((backoff * 2))
        if [ "$backoff" -gt "$MAX_BACKOFF" ]; then
            backoff=$MAX_BACKOFF
        fi
    done

    # Stream journalctl lines; --since "now" avoids replaying old entries
    journalctl -f --no-pager -o short-iso --since "now" | while IFS= read -r line; do
        JSON=$(printf "%s" "$line" | python3 -c "import sys,json; print(json.dumps({\"data\":{\"line\":sys.stdin.read(),\"source\":\"journalctl\"}}))")

        if ! curl -sf -X POST "$TSSTORE_URL/api/stores/$STORE/data" \
            -H "X-API-Key: $API_KEY" \
            -H "Content-Type: application/json" \
            -d "$JSON" > /dev/null 2>&1; then
            echo "curl failed, breaking stream to reconnect..."
            break
        fi
    done

    # journalctl pipe or curl broke — back to health-check loop
    echo "Stream ended, will retry in ${backoff}s..."
    sleep "$backoff"
    backoff=$((backoff * 2))
    if [ "$backoff" -gt "$MAX_BACKOFF" ]; then
        backoff=$MAX_BACKOFF
    fi
done
