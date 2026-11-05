/* Host-only real-process lifecycle fixture, separate from the Go control fixture.
 * Like the deployed C++ server it
 * runs without an application-installed virtual-address cap.
 * It is NOT an inference reference or a substitute for device model tests. */
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

int main(int argc, char **argv) {
    fprintf(stderr, "PICO_HOST_FIXTURE argv0=%s pid=%ld\n", argv[0], (long)getpid());
    int port = 0;
    for (int i = 1; i + 1 < argc; ++i) {
        if (!strcmp(argv[i], "--port")) port = atoi(argv[i + 1]);
    }
    if (port < 1024 || port > 65535) return 9;
    signal(SIGPIPE, SIG_IGN);
    int fd = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (fd < 0) return 8;
    int yes = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &yes, sizeof(yes));
    struct sockaddr_in address = {.sin_family = AF_INET,
        .sin_port = htons((unsigned short)port), .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
    if (bind(fd, (struct sockaddr *)&address, sizeof(address)) || listen(fd, 16)) {
        perror("fixture bind/listen");
        close(fd);
        return 8;
    }
    const char *silent = getenv("PICO_HELPER_NO_BIND_EVENT");
    if (!silent || strcmp(silent, "1")) {
        printf("srv main: listening on http://127.0.0.1:%d\n", port);
        fflush(stdout);
    }
    for (;;) {
        int client = accept4(fd, NULL, NULL, SOCK_CLOEXEC);
        if (client < 0) { if (errno == EINTR) continue; break; }
        struct timeval timeout = {.tv_sec = 2};
        setsockopt(client, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
        setsockopt(client, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
        char request[4096] = {0};
        size_t used = 0;
        while (used < sizeof(request) - 1) {
            ssize_t count = read(client, request + used, sizeof(request) - 1 - used);
            if (count < 0 && errno == EINTR) continue;
            if (count <= 0) break;
            used += (size_t)count;
            request[used] = 0;
            if (strstr(request, "\r\n\r\n")) break;
        }
        const char *response = !strncmp(request, "GET /health ", 12)
            ? "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
            : "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
        size_t left = strlen(response);
        while (left) {
            ssize_t count = write(client, response, left);
            if (count < 0 && errno == EINTR) continue;
            if (count <= 0) break;
            response += count;
            left -= (size_t)count;
        }
        close(client);
    }
    close(fd);
    return 8;
}
