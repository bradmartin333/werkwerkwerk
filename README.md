# werk

A tiny workout challenge. Go + SQLite in one container.

Reps are 5–200. Progress = average across the 3 workouts of the % change from
the first logged day to the most recent one. Only today's entry is editable;
missed days are just gaps. The day rolls over at midnight in `$TZ`.

## Deploy

On the server:

```
sudo mkdir -p /srv/docker-data/werk
sudo chown 65532:65532 /srv/docker-data/werk
docker compose up -d werk
```

To ship a new version after pushing here: `docker compose up -d --build werk`.

## Set up the challenge

```
docker exec werk /werk adduser mom <password>
docker exec werk /werk adduser dad <password>
docker exec werk /werk enddate 2026-11-30
docker exec werk /werk users
```

## Other commands

```
docker exec werk /werk passwd  <name> <password>   # reset password + log them out
docker exec werk /werk deluser <name>              # drops the user and ALL their data
docker exec werk /werk enddate none                # clear the end date
```

## Local run

```
docker build -t werk . && docker run -p 8080:8080 -e TZ=America/New_York werk
```
