# Swerve

## High-Performance URL Redirector


A flexible, high-performance, open-source URL redirection service built with Go and Traefik. This service is designed to handle thousands of redirect rules across multiple domains, making it ideal for complex website migrations and content reorganization.

## Features

 - **Multi-Domain Support:** Manage redirects for multiple source domains from a single service instance.
 - **Automated SSL:** Traefik automatically provisions and renews SSL certificates from Let's Encrypt for all configured domains.
 - **Advanced Rule Matching:**
    - **Regex Matching:** Define redirect rules using powerful regular expressions.
    - **Weighted Rules:** Prioritize which rule applies when multiple regexes could match a given path.
 - **Dynamic URL Rewriting:** Rewrite target URLs on the fly by injecting the original path or capture groups from your regex.
 - **Configuration as Code:** All redirect rules are defined in simple `.csv` files, making them easy to manage, version control, and update.
 - **Containerized:** Fully containerized with Docker and Docker Compose for easy deployment and scalability.
 - **Developer Friendly:** Includes a hot-reloading local development environment powered by `air`


## Architecture Overview

The system consists of two main services orchestrated by Docker Compose:

1. **Traefik:** Acts as the edge router. It terminates SSL, handles all incoming HTTP/HTTPS traffic, and routes requests to the Swerve application based on the requested hostname.
2. **Swerve Redirector App:** A lightweight, high-performance HTTP server that loads all redirect rules from .csv files into memory. For each request, it finds the highest-weighted matching rule and issues the appropriate HTTP redirect


### Request Flow:

![Screenshot of a diagram of how the traffic flows](/assets/diagram.png)


## File Structure

```
/
|-- docker-compose.yml         # Main production configuration
|-- docker-compose.override.yml.dist  # Local development overrides
|-- redirects/                 # Folder for all your redirect rule files
|   |-- old-site.com.csv
|   `-- another-domain.net.csv
|-- swerve/                # Go application source code
|   |-- main.go
|   |-- go.mod
|   |-- go.sum
|   |-- Dockerfile               # Production Dockerfile
|   |-- Dockerfile-dev           # Development Dockerfile
|   `-- .air.toml                # Hot-reloader configuration
`-- README.md
```


## Getting Started

### Prerequisites

 - [Docker](https://www.docker.com/)
 - [Docker compose](https://docs.docker.com/compose/)
 - A server with a public IP address (for production)
 - DNS records for your domains pointing to your server's IP (for production)

### Production Setup

1. **Clone the Repository:**
```
git clone git@github.com:thepearson/swerve.git
cd swerve
```

2. **Configure** `docker-compose.yml`:
    - Open `docker-compose.yml`
    - Update the Traefik `command` section with your email address for Let's Encrypt:
    ```
    - "--certificatesresolvers.myresolver.acme.email=your-email@example.com"
    ```
    - Update the `swerve` labels with the domains you want to manage:
    ```
    - "traefik.http.routers.swerve.rule=Host(`your-domain.com`,`another-domain.org`)"
    ```

3. **Add Redirect Rules:**
    - Create one or more `.csv` files inside the `/redirects` directory. See the "Configuration" section below for the correct format.

4. **Launch the Service:**

    ```
    docker-compose up -d
    ```

    Traefik will now listen for traffic on ports 80 and 443, automatically obtain SSL certificates for your specified domains, and route requests to the redirector application.

### Local Development Setup

The local environment uses the `docker-compose.override.yml` file to enable hot-reloading for the Go application.

1. **Create the development override file**

    ```
    cp docker-compose.override.yml.dist docker-compose.override.yml
    ```
2. **Launch the Development Environment:**

    ```
    # The --build flag is important on the first run to build the dev image
    docker-compose up --build
    ```

3. **Access the Services:**

     * **Redirector App:** `http://localhost` (all requests to this host are routed by Traefik to the app)
     * **Traefik Dashboard:** `http://localhost:8080`

4. **Hot Reloading:**

    * Any changes you make to the Go source code (`.go` files) in the `/swerve` directory will automatically trigger a recompile and restart of the application.
    * Any changes to your redirect rules (`.csv` files) in the `/redirects` directory will also trigger an automatic restart.

## Configuration

**Redirect Rules** (`redirects/*.csv`)

All redirect rules are defined in `.csv` files located in the `/redirects directory`. The service will read and process all `.csv` files in this folder.

Each row in a CSV file represents one rule and must contain 6 columns:

`source_host,match_type,source_path_or_regex,target_url_format,status_code,weight`

 * `source_host`: The domain name the rule applies to (e.g., `old-site.com`).

 * `match_type`: `exact` for a simple path match or `regex` for a regular expression match.

 * `source_path_or_regex`: The exact path or the Go-compatible regular expression to match against the request path.

 * `target_url_format`: The destination URL. This can include placeholders for dynamic rewriting:

    * `$path`: Inserts the entire original path of the request.

    * `$1`, `$2`, etc.: Inserts capture groups from the `regex` match.

 * `status_code`: The HTTP redirect code to use (e.g., `301` for permanent, `302` for temporary).

 * `weight`: An integer. Rules are evaluated from highest weight to lowest. The first rule that matches is the one that gets executed.


 ### **Example** `rules.csv`:

 ```
# A specific blog post redirect with high weight
old-site.com,regex,^/blog/old-post-title$,https://new-site.com/articles/new-post,301,200

# Redirect an entire category using regex capture groups
old-site.com,regex,^/category/(\w+)/?,https://new-site.com/topics/$1,301,150

# Redirect an entire section, keeping the original path
old-site.com,regex,^/support/.*,https://help.new-site.com$path,301,100

# Catch-all for the rest of the domain (lowest weight)
old-site.com,regex,^/.*,https://archive.new-site.com/legacy-content$path,301,10
 ```

## Contributing

Contributions are welcome! Please feel free to submit a pull request or open an issue.

## License

MIT License