FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /werk . && mkdir -p /data

FROM gcr.io/distroless/static:nonroot
COPY --from=build /werk /werk
COPY --from=build --chown=nonroot:nonroot /data /data
ENV WERK_DB=/data/werk.db WERK_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/werk"]
