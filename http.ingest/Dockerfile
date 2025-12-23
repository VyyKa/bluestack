FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/http-ingest ./main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/http-ingest /http-ingest
EXPOSE 9002
USER nonroot:nonroot
ENTRYPOINT ["/http-ingest"]


