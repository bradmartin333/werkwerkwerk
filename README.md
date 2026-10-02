# werk

A tiny family workout challenge. Go + SQLite in one container. Users and the
end date are managed from the CLI inside the container:

```
docker exec werk /werk users
docker exec werk /werk adduser <name> <password>
docker exec werk /werk passwd  <name> <password>
docker exec werk /werk deluser <name>          # drops all their data
docker exec werk /werk enddate 2026-12-31      # or: enddate none
```

Reps are 5–200. Progress = average across the 3 workouts of the % change from the first
logged day to the most recent one. Only today's entry is editable; missed
days are just gaps. The day rolls over at midnight in `$TZ`.

Local run: `docker build -t werk . && docker run -p 8080:8080 -e TZ=America/New_York werk`
