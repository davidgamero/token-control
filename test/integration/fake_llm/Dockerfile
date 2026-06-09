FROM golang:1.23-alpine AS build
WORKDIR /src
COPY main.go .
RUN go build -o /fake-llm .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /fake-llm /fake-llm
EXPOSE 8080
ENTRYPOINT ["/fake-llm"]
