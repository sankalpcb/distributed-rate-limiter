# Build in a pinned Go image, ship on distroless: the final image carries no
# shell and no package manager, which keeps the Cloud Run cold-start image
# small and the attack surface minimal.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off so the binary is static and runs on a scratch-like base.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/limiterd ./cmd/limiterd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/limiterd /limiterd
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/limiterd"]
