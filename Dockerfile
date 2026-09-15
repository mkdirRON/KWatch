# Build the kwatch binary against the module cache, then ship it on a
# scratch image so the runtime container carries nothing but the service.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Copy manifests first so dependency downloads stay cached across code changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

# CGO off keeps the binary static so it runs on scratch.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kwatch ./cmd/kwatch

FROM scratch

COPY --from=build /out/kwatch /kwatch

EXPOSE 9090

ENTRYPOINT ["/kwatch"]
