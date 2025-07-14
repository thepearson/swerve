# Stage 1: Build the Go binary
# Using a specific version of Go ensures consistent builds.
# Updated to match the development environment.
FROM golang:1.24-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum files to download dependencies
# This leverages Docker's layer caching. Dependencies are only re-downloaded
# if these files change.
COPY go.mod ./
COPY go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the binary for a Linux environment, statically linked.
# CGO_ENABLED=0 is important for creating a static binary without C dependencies.
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /swerve .

# Stage 2: Create the final, small production image
# Using alpine gives us a very small and secure base image.
FROM alpine:latest

# It's a good security practice to run as a non-root user.
RUN addgroup -S appuser && adduser -S appuser -G appuser
USER appuser

WORKDIR /app

# Copy the binary from the builder stage
COPY --from=builder /swerve .

# The redirects.csv file will be mounted via a volume in docker-compose,
# so we don't strictly need to copy it here, but it can be useful for testing.

# Expose the port the app runs on
EXPOSE 8080

# The command to run when the container starts
CMD ["/app/swerve"]
