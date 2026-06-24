// Experimental scoped PATH shim for `git rev-parse HEAD`.
//
// This is intentionally narrow: it only serves one command from Squire's
// mmap-backed hot snapshot and execs the real git binary for everything else.
// The launcher must set:
//   SQUIRE_REAL_GIT
//   SQUIRE_REPO_ROOT
//   SQUIRE_GIT_DIR
//   SQUIRE_STORE_ROOT

#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#if defined(__APPLE__)
#include <CommonCrypto/CommonDigest.h>
#define SQUIRE_SHA256_CTX CC_SHA256_CTX
#define SQUIRE_SHA256_Init CC_SHA256_Init
#define SQUIRE_SHA256_Update CC_SHA256_Update
#define SQUIRE_SHA256_Final CC_SHA256_Final
#else
#include <openssl/sha.h>
#define SQUIRE_SHA256_CTX SHA256_CTX
#define SQUIRE_SHA256_Init SHA256_Init
#define SQUIRE_SHA256_Update SHA256_Update
#define SQUIRE_SHA256_Final SHA256_Final
#endif

#define HOT_MAGIC UINT64_C(0x3150535148535153)
#define HOT_VERSION 1
#define HOT_HEADER_BYTES 64
#define HOT_ENTRY_BYTES 320
#define HOT_MAX_ENTRIES 8192
#define HOT_MAX_BYTES (64 * 1024 * 1024)
#define HOT_KIND_EXACT 1

static void exec_real_git(char **argv) {
	const char *real_git = getenv("SQUIRE_REAL_GIT");
	if (real_git == NULL || real_git[0] == '\0') {
		real_git = "git";
	}
	argv[0] = (char *)real_git;
	execv(real_git, argv);
	execvp(real_git, argv);
	fprintf(stderr, "squire shim: exec real git failed: %s\n", strerror(errno));
	_exit(127);
}

static int is_target_command(int argc, char **argv) {
	return argc == 3 &&
	       strcmp(argv[1], "rev-parse") == 0 &&
	       strcmp(argv[2], "HEAD") == 0;
}

static uint16_t le16(const unsigned char *p) {
	return (uint16_t)p[0] | ((uint16_t)p[1] << 8);
}

static uint32_t le32(const unsigned char *p) {
	return (uint32_t)p[0] |
	       ((uint32_t)p[1] << 8) |
	       ((uint32_t)p[2] << 16) |
	       ((uint32_t)p[3] << 24);
}

static uint64_t le64(const unsigned char *p) {
	uint64_t out = 0;
	for (int i = 7; i >= 0; i--) {
		out = (out << 8) | p[i];
	}
	return out;
}

static void sha256_hex_bytes(const unsigned char *data, size_t len, char out[65]) {
	unsigned char digest[32];
	static const char hex[] = "0123456789abcdef";
	SQUIRE_SHA256_CTX ctx;
	SQUIRE_SHA256_Init(&ctx);
	SQUIRE_SHA256_Update(&ctx, data, len);
	SQUIRE_SHA256_Final(digest, &ctx);
	for (int i = 0; i < 32; i++) {
		out[i * 2] = hex[digest[i] >> 4];
		out[i * 2 + 1] = hex[digest[i] & 0x0f];
	}
	out[64] = '\0';
}

static void sha256_hex_str(const char *s, char out[65]) {
	sha256_hex_bytes((const unsigned char *)s, strlen(s), out);
}

static int read_file_trimmed(const char *path, char *buf, size_t cap) {
	int fd = open(path, O_RDONLY);
	if (fd < 0) {
		return 0;
	}
	ssize_t n = read(fd, buf, cap - 1);
	close(fd);
	if (n <= 0) {
		return 0;
	}
	buf[n] = '\0';
	while (n > 0 && (buf[n - 1] == '\n' || buf[n - 1] == '\r' || buf[n - 1] == ' ' || buf[n - 1] == '\t')) {
		buf[n - 1] = '\0';
		n--;
	}
	return 1;
}

static int join_path(char *out, size_t cap, const char *left, const char *right) {
	int n = snprintf(out, cap, "%s/%s", left, right);
	return n > 0 && (size_t)n < cap;
}

static int current_head(const char *git_dir, char head[128]) {
	char head_path[4096];
	char head_file[512];
	if (!join_path(head_path, sizeof(head_path), git_dir, "HEAD")) {
		return 0;
	}
	if (!read_file_trimmed(head_path, head_file, sizeof(head_file))) {
		return 0;
	}
	if (strncmp(head_file, "ref:", 4) != 0) {
		if (strlen(head_file) >= 127) {
			return 0;
		}
		strcpy(head, head_file);
		return 1;
	}
	char *ref = head_file + 4;
	while (*ref == ' ' || *ref == '\t') {
		ref++;
	}
	if (strstr(ref, "..") != NULL || ref[0] == '/' || ref[0] == '\0') {
		return 0;
	}
	char ref_path[4096];
	if (!join_path(ref_path, sizeof(ref_path), git_dir, ref)) {
		return 0;
	}
	return read_file_trimmed(ref_path, head, 128);
}

static int valid_hex64(const char *s) {
	for (int i = 0; i < 64; i++) {
		if (!isxdigit((unsigned char)s[i])) {
			return 0;
		}
	}
	return 1;
}

static int replay_from_snapshot(void) {
	const char *repo_root = getenv("SQUIRE_REPO_ROOT");
	const char *git_dir = getenv("SQUIRE_GIT_DIR");
	const char *store_root = getenv("SQUIRE_STORE_ROOT");
	if (repo_root == NULL || git_dir == NULL || store_root == NULL) {
		return 0;
	}

	char head[128];
	if (!current_head(git_dir, head)) {
		return 0;
	}

	char command_key[65];
	static const unsigned char command_bytes[] = {
		'g', 'i', 't', '\0',
		'r', 'e', 'v', '-', 'p', 'a', 'r', 's', 'e', '\0',
		'H', 'E', 'A', 'D',
	};
	sha256_hex_bytes(command_bytes, sizeof(command_bytes), command_key);

	char repo_head[8192];
	int repo_head_len = snprintf(repo_head, sizeof(repo_head), "%s|%s", repo_root, head);
	if (repo_head_len <= 0 || (size_t)repo_head_len >= sizeof(repo_head)) {
		return 0;
	}
	char repo_head_hash[65];
	sha256_hex_str(repo_head, repo_head_hash);

	char epoch[96];
	int epoch_len = snprintf(epoch, sizeof(epoch), "hot-head:%s", repo_head_hash);
	if (epoch_len <= 0 || (size_t)epoch_len >= sizeof(epoch)) {
		return 0;
	}
	char epoch_hash[65];
	sha256_hex_str(epoch, epoch_hash);

	char snapshot_path[4096];
	if (!join_path(snapshot_path, sizeof(snapshot_path), store_root, "hot_snapshot.bin")) {
		return 0;
	}
	int fd = open(snapshot_path, O_RDONLY);
	if (fd < 0) {
		return 0;
	}
	struct stat st;
	if (fstat(fd, &st) != 0 || st.st_size < HOT_HEADER_BYTES || st.st_size > HOT_MAX_BYTES) {
		close(fd);
		return 0;
	}
	unsigned char *data = mmap(NULL, (size_t)st.st_size, PROT_READ, MAP_SHARED, fd, 0);
	close(fd);
	if (data == MAP_FAILED) {
		return 0;
	}

	int ok = 0;
	if (le64(data) != HOT_MAGIC || le16(data + 8) != HOT_VERSION || le16(data + 10) != HOT_ENTRY_BYTES) {
		goto done;
	}
	uint32_t count = le32(data + 12);
	uint32_t header_size = le32(data + 16);
	uint32_t payload_offset = le32(data + 20);
	uint32_t total_size = le32(data + 24);
	if (count > HOT_MAX_ENTRIES || header_size != HOT_HEADER_BYTES || total_size != (uint32_t)st.st_size ||
	    payload_offset != HOT_HEADER_BYTES + count * HOT_ENTRY_BYTES || payload_offset > total_size) {
		goto done;
	}

	for (uint32_t i = 0; i < count; i++) {
		unsigned char *entry = data + HOT_HEADER_BYTES + i * HOT_ENTRY_BYTES;
		if (memcmp(entry, command_key, 64) != 0 || memcmp(entry + 64, epoch_hash, 64) != 0) {
			continue;
		}
		char stdout_hash[65];
		char stderr_hash[65];
		memcpy(stdout_hash, entry + 128, 64);
		memcpy(stderr_hash, entry + 192, 64);
		stdout_hash[64] = '\0';
		stderr_hash[64] = '\0';
		if (!valid_hex64(stdout_hash) || !valid_hex64(stderr_hash)) {
			goto done;
		}
		uint32_t stdout_offset = le32(entry + 256);
		uint32_t stdout_len = le32(entry + 260);
		uint32_t stderr_offset = le32(entry + 264);
		uint32_t stderr_len = le32(entry + 268);
		int exit_code = (int)(int32_t)le32(entry + 272);
		uint32_t kind = le32(entry + 276);
		if (kind != HOT_KIND_EXACT || stdout_offset < payload_offset || stderr_offset < payload_offset ||
		    stdout_offset + stdout_len > total_size || stderr_offset + stderr_len > total_size) {
			goto done;
		}
		char got_stdout_hash[65];
		char got_stderr_hash[65];
		sha256_hex_bytes(data + stdout_offset, stdout_len, got_stdout_hash);
		sha256_hex_bytes(data + stderr_offset, stderr_len, got_stderr_hash);
		if (strcmp(got_stdout_hash, stdout_hash) != 0 || strcmp(got_stderr_hash, stderr_hash) != 0) {
			goto done;
		}
		if (stdout_len > 0) {
			(void)write(STDOUT_FILENO, data + stdout_offset, stdout_len);
		}
		if (stderr_len > 0) {
			(void)write(STDERR_FILENO, data + stderr_offset, stderr_len);
		}
		ok = 1;
		munmap(data, (size_t)st.st_size);
		_exit(exit_code);
	}

done:
	munmap(data, (size_t)st.st_size);
	return ok;
}

int main(int argc, char **argv) {
	if (!is_target_command(argc, argv)) {
		exec_real_git(argv);
	}
	if (!replay_from_snapshot()) {
		if (getenv("SQUIRE_SHIM_REQUIRE_HIT") != NULL) {
			fprintf(stderr, "squire shim: hot snapshot miss\n");
			return 91;
		}
		exec_real_git(argv);
	}
	return 0;
}
