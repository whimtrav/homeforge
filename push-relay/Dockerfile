# Build the HomeForge push relay. go.sum is generated during the build (go mod tidy),
# so the repo only needs go.mod + the source.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /relay .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /relay /relay
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/relay"]
