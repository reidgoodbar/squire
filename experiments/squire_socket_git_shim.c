// Experimental scoped PATH shim that sends git argv to a resident Squire helper.
//
// This measures the "reuse the adapter path" bridge without spawning a Squire
// adapter per command. The launcher must set:
//   SQUIRE_REAL_GIT
//   SQUIRE_SHIM_SOCKET

#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#define FRAME_HEADER_BYTES 24
#define FRAME_MAX_OUTPUT_BYTES (64 * 1024 * 1024)

static const unsigned char FRAME_MAGIC[8] = {'S', 'Q', 'S', 'H', 'I', 'M', '1', 0};

static void exec_real_git(char **argv) {
	const char *real_git = getenv("SQUIRE_REAL_GIT");
	if (real_git == NULL || real_git[0] == '\0') {
		real_git = "git";
	}
	argv[0] = (char *)real_git;
	execv(real_git, argv);
	execvp(real_git, argv);
	fprintf(stderr, "squire socket shim: exec real git failed: %s\n", strerror(errno));
	_exit(127);
}

static uint32_t le32(const unsigned char *p) {
	return (uint32_t)p[0] |
	       ((uint32_t)p[1] << 8) |
	       ((uint32_t)p[2] << 16) |
	       ((uint32_t)p[3] << 24);
}

static int write_all(int fd, const void *buf, size_t len) {
	const unsigned char *p = (const unsigned char *)buf;
	while (len > 0) {
		ssize_t n = write(fd, p, len);
		if (n < 0) {
			if (errno == EINTR) {
				continue;
			}
			return 0;
		}
		p += n;
		len -= (size_t)n;
	}
	return 1;
}

static int read_all(int fd, void *buf, size_t len) {
	unsigned char *p = (unsigned char *)buf;
	while (len > 0) {
		ssize_t n = read(fd, p, len);
		if (n == 0) {
			return 0;
		}
		if (n < 0) {
			if (errno == EINTR) {
				continue;
			}
			return 0;
		}
		p += n;
		len -= (size_t)n;
	}
	return 1;
}

static int connect_socket(const char *path) {
	int fd = socket(AF_UNIX, SOCK_STREAM, 0);
	if (fd < 0) {
		return -1;
	}
	struct sockaddr_un addr;
	memset(&addr, 0, sizeof(addr));
	addr.sun_family = AF_UNIX;
	if (strlen(path) >= sizeof(addr.sun_path)) {
		close(fd);
		return -1;
	}
	strcpy(addr.sun_path, path);
	if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
		close(fd);
		return -1;
	}
	return fd;
}

static int send_request(int fd, int argc, char **argv) {
	char cwd[4096];
	if (getcwd(cwd, sizeof(cwd)) == NULL) {
		return 0;
	}
	if (!write_all(fd, cwd, strlen(cwd) + 1)) {
		return 0;
	}
	if (!write_all(fd, "git", 4)) {
		return 0;
	}
	for (int i = 1; i < argc; i++) {
		if (!write_all(fd, argv[i], strlen(argv[i]) + 1)) {
			return 0;
		}
	}
	return 1;
}

static int serve_from_helper(int argc, char **argv) {
	const char *socket_path = getenv("SQUIRE_SHIM_SOCKET");
	if (socket_path == NULL || socket_path[0] == '\0') {
		return 0;
	}
	int fd = connect_socket(socket_path);
	if (fd < 0) {
		return 0;
	}
	if (!send_request(fd, argc, argv)) {
		close(fd);
		return 0;
	}
	shutdown(fd, SHUT_WR);

	unsigned char header[FRAME_HEADER_BYTES];
	if (!read_all(fd, header, sizeof(header))) {
		close(fd);
		return 0;
	}
	if (memcmp(header, FRAME_MAGIC, sizeof(FRAME_MAGIC)) != 0) {
		close(fd);
		return 0;
	}
	int exit_code = (int)(int32_t)le32(header + 8);
	uint32_t mode = le32(header + 12);
	uint32_t stdout_len = le32(header + 16);
	uint32_t stderr_len = le32(header + 20);
	if ((uint64_t)stdout_len + (uint64_t)stderr_len > FRAME_MAX_OUTPUT_BYTES) {
		close(fd);
		return 0;
	}
	unsigned char *payload = NULL;
	if (stdout_len + stderr_len > 0) {
		payload = (unsigned char *)malloc((size_t)stdout_len + (size_t)stderr_len);
		if (payload == NULL) {
			close(fd);
			return 0;
		}
		if (!read_all(fd, payload, (size_t)stdout_len + (size_t)stderr_len)) {
			free(payload);
			close(fd);
			return 0;
		}
	}
	close(fd);

	if (getenv("SQUIRE_SHIM_REQUIRE_HIT") != NULL && mode != 1) {
		free(payload);
		fprintf(stderr, "squire socket shim: replay miss\n");
		return 91;
	}
	if (stdout_len > 0) {
		(void)write_all(STDOUT_FILENO, payload, stdout_len);
	}
	if (stderr_len > 0) {
		(void)write_all(STDERR_FILENO, payload + stdout_len, stderr_len);
	}
	free(payload);
	_exit(exit_code);
}

int main(int argc, char **argv) {
	int rc = serve_from_helper(argc, argv);
	if (rc != 0) {
		return rc;
	}
	exec_real_git(argv);
	return 127;
}
