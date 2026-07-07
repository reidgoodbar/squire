// Internal mmap proof engine used by Squire's preload transport.
//
// The shim is intentionally local and fault-open. It serves only entries whose
// current invalidation proof can be recomputed in this process. Everything else
// execs the real command with no semantic approximation.
//
// Supported direct-mmap surfaces:
//   - enabled Git metadata fast paths
//   - proof-gated Git repo summaries: ls-files, status, diff
//   - warmed bounded file inspection: cat/head/tail <file>, sed -n <range>p <file>
//   - warmed literal grep/rg checks and native-precomputed file(1) type inspection
//   - common tool version probes and command path lookups
//   - static environment probes, printenv <safe-var>, and tight directory listings
//
// Required/optional launcher environment:
//   SQUIRE_STORE_ROOT       optional; otherwise discovered as <gitdir>/squire/kernel
//   SQUIRE_SHIM_REAL_PATH   optional native PATH used for proof and fallback
//   SQUIRE_REAL_<TOOL>      optional exact native binary path, e.g. SQUIRE_REAL_GIT

#include <ctype.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/time.h>
#include <time.h>
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
#define HOT_KIND_WARM_FILE 2
#define HOT_CLIENT_STATS_MAX_BYTES (1024 * 1024)
#define MAX_FAST_OUTPUT_BYTES (64 * 1024)
#define MAX_WARM_FILE_BYTES (256 * 1024)
#define MAX_EXECUTABLE_HASH_BYTES (64 * 1024 * 1024)
#define MAX_ARGC 64
#define PATH_BUF 4096
#define HASH_HEX 65
#define HOT_CLIENT_PROOF_C_MMAP "c-mmap-hot-snapshot"
#define HOT_CLIENT_PROOF_C_SYNTHETIC "c-mmap-hot-synthetic"

typedef struct {
	unsigned char *data;
	size_t len;
} byte_item;

typedef struct {
	byte_item *items;
	size_t len;
	size_t cap;
} string_list;

typedef struct {
	unsigned char *data;
	size_t len;
	size_t cap;
} byte_buf;

typedef struct {
	char path_hash[HASH_HEX];
	char file_hash[HASH_HEX];
} executable_signal;

typedef struct {
	char cwd[PATH_BUF];
	int argc;
	char *argv[MAX_ARGC];
	char storage[MAX_ARGC][PATH_BUF];
} policy_invocation;

typedef struct {
	unsigned char *data;
	size_t len;
	int borrowed;
} mapped_snapshot;

typedef struct {
	mapped_snapshot snap;
	const unsigned char *stdout_data;
	const unsigned char *stderr_data;
	uint32_t stdout_len;
	uint32_t stderr_len;
	int exit_code;
	int synthetic_safe;
	uint64_t native_wall_ms;
	char store_root[PATH_BUF];
	long long replay_start_ns;
} prepared_exact_replay;

static long long stat_mtime_nano(const struct stat *st);
static int file_stat_signal(const struct stat *st, const char *mode, char *out, size_t cap);
static int join_path(char *out, size_t cap, const char *left, const char *right);
static int safe_relative_inspection_path_arg(const char *path);

static const char *base_name(const char *path) {
	const char *slash = strrchr(path, '/');
	return slash ? slash + 1 : path;
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

static int write_event_best_effort(int fd, const void *buf, size_t len) {
	for (;;) {
		ssize_t n = write(fd, buf, len);
		if (n < 0 && errno == EINTR) {
			continue;
		}
		return n == (ssize_t)len;
	}
}

static long long now_realtime_ns(void) {
#if defined(CLOCK_REALTIME)
	struct timespec ts;
	if (clock_gettime(CLOCK_REALTIME, &ts) == 0) {
		return (long long)ts.tv_sec * 1000000000LL + (long long)ts.tv_nsec;
	}
#endif
	struct timeval tv;
	if (gettimeofday(&tv, NULL) == 0) {
		return (long long)tv.tv_sec * 1000000000LL + (long long)tv.tv_usec * 1000LL;
	}
	return 0;
}

static long long now_monotonic_ns(void) {
#if defined(CLOCK_MONOTONIC)
	struct timespec ts;
	if (clock_gettime(CLOCK_MONOTONIC, &ts) == 0) {
		return (long long)ts.tv_sec * 1000000000LL + (long long)ts.tv_nsec;
	}
#endif
	return now_realtime_ns();
}

static int mmap_trace_enabled(void) {
	static int cached = -1;
	if (cached >= 0) {
		return cached;
	}
	const char *value = getenv("SQUIRE_PRELOAD_TRACE");
	if (value == NULL || value[0] == '\0' || strcmp(value, "0") == 0) {
		value = getenv("SQUIRE_SHIM_DEBUG");
	}
	cached = value != NULL && value[0] != '\0' && strcmp(value, "0") != 0;
	return cached;
}

static void mmap_trace_path(const char *event, const char *path) {
	if (!mmap_trace_enabled()) {
		return;
	}
	write_all(STDERR_FILENO, "squire mmap trace: ", 19);
	write_all(STDERR_FILENO, event, strlen(event));
	if (path != NULL) {
		write_all(STDERR_FILENO, " ", 1);
		write_all(STDERR_FILENO, path, strlen(path));
	}
	write_all(STDERR_FILENO, "\n", 1);
}

static void mmap_trace_errno_path(const char *event, const char *path, int errnum) {
	if (!mmap_trace_enabled()) {
		return;
	}
	char prefix[96];
	int n = snprintf(prefix, sizeof(prefix), "squire mmap trace: %s errno=%d", event, errnum);
	if (n > 0 && n < (int)sizeof(prefix)) {
		write_all(STDERR_FILENO, prefix, (size_t)n);
	}
	if (path != NULL) {
		write_all(STDERR_FILENO, " ", 1);
		write_all(STDERR_FILENO, path, strlen(path));
	}
	write_all(STDERR_FILENO, "\n", 1);
}

static int hot_event_fd(void) {
	static int cached = -2;
	if (cached != -2) {
		return cached;
	}
	const char *raw = getenv("SQUIRE_HOT_EVENT_FD");
	if (raw == NULL || raw[0] == '\0') {
		cached = -1;
		return cached;
	}
	char *end = NULL;
	errno = 0;
	long parsed = strtol(raw, &end, 10);
	if (errno != 0 || end == raw || *end != '\0' || parsed < 3 || parsed > INT_MAX) {
		cached = -1;
		return cached;
	}
	cached = (int)parsed;
	return cached;
}

static int hot_snapshot_fd(void) {
	static int cached = -2;
	if (cached != -2) {
		return cached;
	}
	const char *raw = getenv("SQUIRE_HOT_SNAPSHOT_FD");
	if (raw == NULL || raw[0] == '\0') {
		cached = -1;
		return cached;
	}
	char *end = NULL;
	errno = 0;
	long parsed = strtol(raw, &end, 10);
	if (errno != 0 || end == raw || *end != '\0' || parsed < 3 || parsed > INT_MAX) {
		cached = -1;
		return cached;
	}
	cached = (int)parsed;
	return cached;
}

static int mkdir_p(const char *path) {
	if (path == NULL || path[0] == '\0') {
		return 0;
	}
	char tmp[PATH_BUF];
	snprintf(tmp, sizeof(tmp), "%s", path);
	size_t len = strlen(tmp);
	if (len == 0 || len >= sizeof(tmp)) {
		return 0;
	}
	if (len > 1 && tmp[len - 1] == '/') {
		tmp[len - 1] = '\0';
	}
	for (char *p = tmp + 1; *p != '\0'; p++) {
		if (*p != '/') {
			continue;
		}
		*p = '\0';
		if (mkdir(tmp, 0700) != 0 && errno != EEXIST) {
			return 0;
		}
		*p = '/';
	}
	return mkdir(tmp, 0700) == 0 || errno == EEXIST;
}

static void record_hot_replay_event_kind(const char *store_root, const char *proof, long long native_wall_ms, long long replay_start_ns) {
	if (proof == NULL || proof[0] == '\0') {
		proof = HOT_CLIENT_PROOF_C_MMAP;
	}
	long long replay_us = 0;
	long long now_mono = now_monotonic_ns();
	if (replay_start_ns > 0 && now_mono > replay_start_ns) {
		replay_us = (now_mono - replay_start_ns) / 1000LL;
	}
	if (replay_us <= 0) {
		replay_us = 1;
	}
	char line[256];
	int n = snprintf(line, sizeof(line), "%lld replay %s %lld %lld\n",
	                 now_realtime_ns(), proof, native_wall_ms, replay_us);
	if (n <= 0 || n >= (int)sizeof(line)) {
		return;
	}
	int event_fd = hot_event_fd();
	if (event_fd >= 0) {
		if (write_event_best_effort(event_fd, line, (size_t)n)) {
			mmap_trace_path("event-write-fd-ok", NULL);
		} else {
			mmap_trace_errno_path("event-write-fd-dropped", NULL, errno);
		}
		return;
	}
	if (store_root == NULL || store_root[0] == '\0' || getenv("SQUIRE_SHIM_DISABLE_EVENT_LOG") != NULL) {
		mmap_trace_path("event-write-skip-disabled", store_root);
		return;
	}
	if (!mkdir_p(store_root)) {
		mmap_trace_path("event-write-skip-mkdir", store_root);
		return;
	}
	char event_path[PATH_BUF];
	if (!join_path(event_path, sizeof(event_path), store_root, "hot_client_events.log")) {
		mmap_trace_path("event-write-skip-path", store_root);
		return;
	}
	struct stat st;
	if (stat(event_path, &st) == 0 && st.st_size >= HOT_CLIENT_STATS_MAX_BYTES) {
		mmap_trace_path("event-write-skip-full", event_path);
		return;
	}
	int fd = open(event_path, O_CREAT | O_WRONLY | O_APPEND, 0600);
	if (fd < 0) {
		mmap_trace_errno_path("event-write-skip-open", event_path, errno);
		return;
	}
	if (write_all(fd, line, (size_t)n)) {
		mmap_trace_path("event-write-ok", event_path);
	} else {
		mmap_trace_errno_path("event-write-skip-write", event_path, errno);
	}
	close(fd);
}

static void record_hot_replay_event(const char *store_root, long long native_wall_ms, long long replay_start_ns) {
	record_hot_replay_event_kind(store_root, HOT_CLIENT_PROOF_C_MMAP, native_wall_ms, replay_start_ns);
}

static void env_key_for_tool(const char *tool, char out[128]) {
	snprintf(out, 128, "SQUIRE_REAL_");
	size_t off = strlen(out);
	for (size_t i = 0; tool[i] != '\0' && off + 1 < 128; i++) {
		unsigned char c = (unsigned char)tool[i];
		out[off++] = (char)(isalnum(c) ? toupper(c) : '_');
	}
	out[off] = '\0';
}

#if !defined(SQUIRE_MMAP_EMBEDDED) || defined(SQUIRE_MMAP_HELPER_REAL_EXEC)
static void exec_real_command(int argc, char **argv) {
	const char *tool = base_name(argv[0]);
	char env_key[128];
	env_key_for_tool(tool, env_key);
	const char *real = getenv(env_key);
	if (real == NULL || real[0] == '\0') {
		if (strcmp(tool, "git") == 0) {
			real = getenv("SQUIRE_REAL_GIT");
		} else if (strcmp(tool, "cat") == 0) {
			real = getenv("SQUIRE_REAL_CAT");
		} else if (strcmp(tool, "sed") == 0) {
			real = getenv("SQUIRE_REAL_SED");
		}
	}
	if (real != NULL && real[0] != '\0') {
		argv[0] = (char *)real;
		execv(real, argv);
	}
	const char *real_path = getenv("SQUIRE_SHIM_REAL_PATH");
	if (real_path != NULL && real_path[0] != '\0') {
		setenv("PATH", real_path, 1);
	}
	argv[0] = (char *)tool;
	execvp(tool, argv);
	fprintf(stderr, "squire mmap proof: exec native %s failed: %s\n", tool, strerror(errno));
	_exit(127);
}
#endif

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

static void sha256_hex_bytes(const unsigned char *data, size_t len, char out[HASH_HEX]) {
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

static void sha256_hex_str(const char *s, char out[HASH_HEX]) {
	sha256_hex_bytes((const unsigned char *)s, strlen(s), out);
}

static int bytes_append(byte_buf *b, const void *data, size_t len) {
	if (len == 0) {
		return 1;
	}
	if (b->len + len < b->len) {
		return 0;
	}
	if (b->len + len > b->cap) {
		size_t next = b->cap == 0 ? 256 : b->cap * 2;
		while (next < b->len + len) {
			next *= 2;
		}
		unsigned char *p = (unsigned char *)realloc(b->data, next);
		if (p == NULL) {
			return 0;
		}
		b->data = p;
		b->cap = next;
	}
	memcpy(b->data + b->len, data, len);
	b->len += len;
	return 1;
}

static int bytes_append_str(byte_buf *b, const char *s) {
	return bytes_append(b, s, strlen(s));
}

static int bytes_append_byte(byte_buf *b, unsigned char c) {
	return bytes_append(b, &c, 1);
}

static void bytes_free(byte_buf *b) {
	free(b->data);
	b->data = NULL;
	b->len = 0;
	b->cap = 0;
}

static void sha256_hex_buf(byte_buf *b, char out[HASH_HEX]) {
	sha256_hex_bytes(b->data, b->len, out);
}

static int bytes_append_argv_norm(byte_buf *b, policy_invocation *inv) {
	for (int i = 0; i < inv->argc; i++) {
		if (i > 0 && !bytes_append_byte(b, 0)) {
			return 0;
		}
		if (!bytes_append_str(b, inv->argv[i])) {
			return 0;
		}
	}
	return 1;
}

static int hash_argv_norm(policy_invocation *inv, char out[HASH_HEX]) {
	byte_buf b = {0};
	int ok = bytes_append_argv_norm(&b, inv);
	if (ok) {
		sha256_hex_buf(&b, out);
	}
	bytes_free(&b);
	return ok;
}

static int list_add_bytes(string_list *list, const void *data, size_t len) {
	if (list->len == list->cap) {
		size_t next = list->cap == 0 ? 64 : list->cap * 2;
		byte_item *items = (byte_item *)realloc(list->items, next * sizeof(byte_item));
		if (items == NULL) {
			return 0;
		}
		list->items = items;
		list->cap = next;
	}
	list->items[list->len].data = (unsigned char *)malloc(len == 0 ? 1 : len);
	if (list->items[list->len].data == NULL) {
		return 0;
	}
	memcpy(list->items[list->len].data, data, len);
	list->items[list->len].len = len;
	list->len++;
	return 1;
}

static int list_add(string_list *list, const char *s) {
	return list_add_bytes(list, s, strlen(s));
}

static int cmp_byte_item(const void *a, const void *b) {
	const byte_item *ia = (const byte_item *)a;
	const byte_item *ib = (const byte_item *)b;
	size_t min = ia->len < ib->len ? ia->len : ib->len;
	int cmp = memcmp(ia->data, ib->data, min);
	if (cmp != 0) {
		return cmp;
	}
	if (ia->len < ib->len) {
		return -1;
	}
	if (ia->len > ib->len) {
		return 1;
	}
	return 0;
}

static void list_sort(string_list *list) {
	qsort(list->items, list->len, sizeof(byte_item), cmp_byte_item);
}

static void list_free(string_list *list) {
	for (size_t i = 0; i < list->len; i++) {
		free(list->items[i].data);
	}
	free(list->items);
	list->items = NULL;
	list->len = 0;
	list->cap = 0;
}

static int hash_joined_lines(string_list *list, char out[HASH_HEX]) {
	list_sort(list);
	byte_buf b = {0};
	for (size_t i = 0; i < list->len; i++) {
		if (i > 0 && !bytes_append_byte(&b, '\n')) {
			bytes_free(&b);
			return 0;
		}
		if (!bytes_append(&b, list->items[i].data, list->items[i].len)) {
			bytes_free(&b);
			return 0;
		}
	}
	sha256_hex_buf(&b, out);
	bytes_free(&b);
	return 1;
}

static int hash_joined_lines_ordered(string_list *list, char out[HASH_HEX]) {
	byte_buf b = {0};
	for (size_t i = 0; i < list->len; i++) {
		if (i > 0 && !bytes_append_byte(&b, '\n')) {
			bytes_free(&b);
			return 0;
		}
		if (!bytes_append(&b, list->items[i].data, list->items[i].len)) {
			bytes_free(&b);
			return 0;
		}
	}
	sha256_hex_buf(&b, out);
	bytes_free(&b);
	return 1;
}

static int valid_hex64(const char *s) {
	for (int i = 0; i < 64; i++) {
		if (!isxdigit((unsigned char)s[i])) {
			return 0;
		}
	}
	return 1;
}

static int env_truthy(const char *name) {
	const char *value = getenv(name);
	if (value == NULL || value[0] == '\0' || strcmp(value, "0") == 0 || strcasecmp(value, "false") == 0 || strcasecmp(value, "no") == 0) {
		return 0;
	}
	return 1;
}

static int join_path(char *out, size_t cap, const char *left, const char *right) {
	if (left == NULL || left[0] == '\0' || right == NULL || right[0] == '\0') {
		return 0;
	}
	int n;
	if (left[strlen(left) - 1] == '/') {
		n = snprintf(out, cap, "%s%s", left, right);
	} else {
		n = snprintf(out, cap, "%s/%s", left, right);
	}
	return n > 0 && (size_t)n < cap;
}

static int absolute_path(const char *path, const char *cwd, char out[PATH_BUF]) {
	if (path == NULL || path[0] == '\0') {
		return 0;
	}
	if (path[0] == '/') {
		snprintf(out, PATH_BUF, "%s", path);
		return out[0] != '\0';
	}
	char joined[PATH_BUF];
	if (!join_path(joined, sizeof(joined), cwd, path)) {
		return 0;
	}
	snprintf(out, PATH_BUF, "%s", joined);
	return 1;
}

static int clean_relative_path(const char *input, char out[PATH_BUF]) {
	if (input == NULL || input[0] == '\0' || input[0] == '-' || input[0] == '/') {
		return 0;
	}
	char tmp[PATH_BUF];
	snprintf(tmp, sizeof(tmp), "%s", input);
	char *parts[256];
	int count = 0;
	char *save = NULL;
	for (char *part = strtok_r(tmp, "/", &save); part != NULL; part = strtok_r(NULL, "/", &save)) {
		if (part[0] == '\0' || strcmp(part, ".") == 0) {
			continue;
		}
		if (strcmp(part, "..") == 0) {
			if (count == 0) {
				return 0;
			}
			count--;
			continue;
		}
		if (strcmp(part, ".git") == 0 || strcmp(part, ".squire") == 0 || part[0] == '.') {
			return 0;
		}
		if (count >= 256) {
			return 0;
		}
		parts[count++] = part;
	}
	if (count == 0) {
		return 0;
	}
	out[0] = '\0';
	for (int i = 0; i < count; i++) {
		size_t used = strlen(out);
		int n = snprintf(out + used, PATH_BUF - used, "%s%s", i > 0 ? "/" : "", parts[i]);
		if (n < 0 || (size_t)n >= PATH_BUF - used) {
			return 0;
		}
	}
	return out[0] != '\0';
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

static int read_file_hash(const char *path, char out[HASH_HEX], unsigned char **content, size_t *content_len) {
	int fd = open(path, O_RDONLY);
	if (fd < 0) {
		return 0;
	}
	struct stat st;
	if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size < 0 || st.st_size > MAX_WARM_FILE_BYTES) {
		close(fd);
		return 0;
	}
	if (content == NULL) {
		unsigned char digest[32];
		static const char hex[] = "0123456789abcdef";
		unsigned char buf[16384];
		SQUIRE_SHA256_CTX ctx;
		SQUIRE_SHA256_Init(&ctx);
		for (;;) {
			ssize_t n = read(fd, buf, sizeof(buf));
			if (n < 0) {
				if (errno == EINTR) {
					continue;
				}
				close(fd);
				return 0;
			}
			if (n == 0) {
				break;
			}
			SQUIRE_SHA256_Update(&ctx, buf, (size_t)n);
		}
		close(fd);
		SQUIRE_SHA256_Final(digest, &ctx);
		for (int i = 0; i < 32; i++) {
			out[i * 2] = hex[digest[i] >> 4];
			out[i * 2 + 1] = hex[digest[i] & 0x0f];
		}
		out[64] = '\0';
		return 1;
	}
	unsigned char *buf = NULL;
	if (st.st_size > 0) {
		buf = (unsigned char *)malloc((size_t)st.st_size);
		if (buf == NULL) {
			close(fd);
			return 0;
		}
		size_t off = 0;
		while (off < (size_t)st.st_size) {
			ssize_t n = read(fd, buf + off, (size_t)st.st_size - off);
			if (n < 0) {
				if (errno == EINTR) {
					continue;
				}
				free(buf);
				close(fd);
				return 0;
			}
			if (n == 0) {
				free(buf);
				close(fd);
				return 0;
			}
			off += (size_t)n;
		}
	}
	close(fd);
	sha256_hex_bytes(buf, (size_t)st.st_size, out);
	*content = buf;
	if (content_len != NULL) {
		*content_len = (size_t)st.st_size;
	}
	return 1;
}

static int read_executable_hash(const char *path, char out[HASH_HEX]) {
	int fd = open(path, O_RDONLY);
	if (fd < 0) {
		return 0;
	}
	struct stat st;
	if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size < 0 || st.st_size > MAX_EXECUTABLE_HASH_BYTES) {
		close(fd);
		return 0;
	}
	unsigned char digest[32];
	static const char hex[] = "0123456789abcdef";
	unsigned char buf[16384];
	SQUIRE_SHA256_CTX ctx;
	SQUIRE_SHA256_Init(&ctx);
	for (;;) {
		ssize_t n = read(fd, buf, sizeof(buf));
		if (n < 0) {
			if (errno == EINTR) {
				continue;
			}
			close(fd);
			return 0;
		}
		if (n == 0) {
			break;
		}
		SQUIRE_SHA256_Update(&ctx, buf, (size_t)n);
	}
	close(fd);
	SQUIRE_SHA256_Final(digest, &ctx);
	for (int i = 0; i < 32; i++) {
		out[i * 2] = hex[digest[i] >> 4];
		out[i * 2 + 1] = hex[digest[i] & 0x0f];
	}
	out[64] = '\0';
	return 1;
}

static int read_packed_ref(const char *git_dir, const char *ref, char out[128]) {
	char packed_path[PATH_BUF];
	if (!join_path(packed_path, sizeof(packed_path), git_dir, "packed-refs")) {
		return 0;
	}
	FILE *f = fopen(packed_path, "r");
	if (f == NULL) {
		return 0;
	}
	char line[1024];
	int ok = 0;
	while (fgets(line, sizeof(line), f) != NULL) {
		char *p = line;
		while (*p == ' ' || *p == '\t') {
			p++;
		}
		if (*p == '\0' || *p == '#' || *p == '^') {
			continue;
		}
		char hash[128], name[512];
		if (sscanf(p, "%127s %511s", hash, name) == 2 && strcmp(name, ref) == 0) {
			snprintf(out, 128, "%s", hash);
			ok = 1;
			break;
		}
	}
	fclose(f);
	return ok;
}

static int current_head_and_branch(const char *git_dir, char head[128], char branch[PATH_BUF]) {
	char head_path[PATH_BUF];
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
		strcpy(branch, "HEAD");
		return 1;
	}
	char *ref = head_file + 4;
	while (*ref == ' ' || *ref == '\t') {
		ref++;
	}
	if (strstr(ref, "..") != NULL || ref[0] == '/' || ref[0] == '\0') {
		return 0;
	}
	char ref_path[PATH_BUF];
	if (!join_path(ref_path, sizeof(ref_path), git_dir, ref)) {
		return 0;
	}
	if (!read_file_trimmed(ref_path, head, 128) && !read_packed_ref(git_dir, ref, head)) {
		return 0;
	}
	const char *prefix = "refs/heads/";
	if (strncmp(ref, prefix, strlen(prefix)) == 0) {
		snprintf(branch, PATH_BUF, "%s", ref + strlen(prefix));
	} else {
		snprintf(branch, PATH_BUF, "%s", ref);
	}
	return 1;
}

static int discover_git_dir(const char *cwd, char repo_root[PATH_BUF], char git_dir[PATH_BUF]) {
	char dir[PATH_BUF];
	snprintf(dir, sizeof(dir), "%s", cwd);
	for (;;) {
		char git_path[PATH_BUF];
		if (!join_path(git_path, sizeof(git_path), dir, ".git")) {
			return 0;
		}
		struct stat st;
		if (stat(git_path, &st) == 0) {
			if (S_ISDIR(st.st_mode)) {
				snprintf(repo_root, PATH_BUF, "%s", dir);
				snprintf(git_dir, PATH_BUF, "%s", git_path);
				return 1;
			}
			if (S_ISREG(st.st_mode)) {
				char text[PATH_BUF];
				if (read_file_trimmed(git_path, text, sizeof(text)) && strncmp(text, "gitdir:", 7) == 0) {
					char *target = text + 7;
					while (*target == ' ' || *target == '\t') {
						target++;
					}
					snprintf(repo_root, PATH_BUF, "%s", dir);
					if (target[0] == '/') {
						snprintf(git_dir, PATH_BUF, "%s", target);
					} else {
						join_path(git_dir, PATH_BUF, dir, target);
					}
					return 1;
				}
			}
		}
		char *slash = strrchr(dir, '/');
		if (slash == NULL || slash == dir) {
			return 0;
		}
		*slash = '\0';
	}
}

static int mode_string(mode_t mode, char out[32]) {
	if (S_ISREG(mode)) {
		out[0] = '-';
	} else if (S_ISDIR(mode)) {
		out[0] = 'd';
	} else if (S_ISLNK(mode)) {
		out[0] = 'L';
	} else {
		return 0;
	}
	out[1] = (mode & S_IRUSR) ? 'r' : '-';
	out[2] = (mode & S_IWUSR) ? 'w' : '-';
	out[3] = (mode & S_IXUSR) ? 'x' : '-';
	out[4] = (mode & S_IRGRP) ? 'r' : '-';
	out[5] = (mode & S_IWGRP) ? 'w' : '-';
	out[6] = (mode & S_IXGRP) ? 'x' : '-';
	out[7] = (mode & S_IROTH) ? 'r' : '-';
	out[8] = (mode & S_IWOTH) ? 'w' : '-';
	out[9] = (mode & S_IXOTH) ? 'x' : '-';
	out[10] = '\0';
	return 1;
}

static int rel_git_dir(const char *cwd, const char *git_dir, char out[PATH_BUF]) {
	size_t cwd_len = strlen(cwd);
	if (strcmp(cwd, git_dir) == 0) {
		strcpy(out, ".");
		return 1;
	}
	if (strncmp(git_dir, cwd, cwd_len) == 0 && git_dir[cwd_len] == '/') {
		snprintf(out, PATH_BUF, "%s", git_dir + cwd_len + 1);
		return 1;
	}
	if (strstr(git_dir, "/.git") != NULL) {
		snprintf(out, PATH_BUF, ".git");
		return 1;
	}
	snprintf(out, PATH_BUF, "%s", git_dir);
	return 1;
}

static const char *proof_path_env(void) {
	const char *real_path = getenv("SQUIRE_SHIM_REAL_PATH");
	if (real_path != NULL && real_path[0] != '\0') {
		return real_path;
	}
	const char *path = getenv("PATH");
	return path != NULL ? path : "";
}

static int explicit_real_tool_path(const char *name, char out[PATH_BUF]) {
	char env_key[128];
	env_key_for_tool(name, env_key);
	const char *real = getenv(env_key);
	if ((real == NULL || real[0] == '\0') && strcmp(name, "git") == 0) {
		real = getenv("SQUIRE_REAL_GIT");
	}
	if (real == NULL || real[0] == '\0') {
		return 0;
	}
	char resolved[PATH_BUF];
	if (realpath(real, resolved) != NULL) {
		snprintf(out, PATH_BUF, "%s", resolved);
		return 1;
	}
	snprintf(out, PATH_BUF, "%s", real);
	return 1;
}

static int resolve_executable(const char *cwd, const char *name, char out[PATH_BUF]) {
	if (name == NULL || name[0] == '\0' || strchr(name, '/') != NULL) {
		return 0;
	}
	if (explicit_real_tool_path(name, out)) {
		return 1;
	}
	char path_copy[PATH_BUF * 4];
	snprintf(path_copy, sizeof(path_copy), "%s", proof_path_env());
	if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
		fprintf(stderr, "squire mmap proof debug: resolve file path_env=%s\n", path_copy);
	}
	char *save = NULL;
	for (char *dir = strtok_r(path_copy, ":", &save); dir != NULL; dir = strtok_r(NULL, ":", &save)) {
		char absdir[PATH_BUF];
		if (dir[0] == '\0') {
			snprintf(absdir, sizeof(absdir), "%s", cwd);
		} else if (dir[0] == '/') {
			snprintf(absdir, sizeof(absdir), "%s", dir);
		} else if (!join_path(absdir, sizeof(absdir), cwd, dir)) {
			continue;
		}
		char candidate[PATH_BUF];
		if (!join_path(candidate, sizeof(candidate), absdir, name)) {
			continue;
		}
		struct stat st;
		if (stat(candidate, &st) != 0 || S_ISDIR(st.st_mode) || (st.st_mode & 0111) == 0) {
			if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
				fprintf(stderr, "squire mmap proof debug: resolve file reject candidate=%s errno=%d\n", candidate, errno);
			}
			continue;
		}
		char resolved[PATH_BUF];
		if (realpath(candidate, resolved) != NULL) {
			snprintf(out, PATH_BUF, "%s", resolved);
		} else {
			snprintf(out, PATH_BUF, "%s", candidate);
		}
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
			fprintf(stderr, "squire mmap proof debug: resolve file accept candidate=%s resolved=%s\n", candidate, out);
		}
		return 1;
	}
	if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
		fprintf(stderr, "squire mmap proof debug: resolve file exhausted\n");
	}
	return 0;
}

static int executable_signal_for(const char *cwd, const char *name, executable_signal *sig) {
	char path[PATH_BUF];
	if (!resolve_executable(cwd, name, path)) {
		return 0;
	}
	struct stat st;
	if (stat(path, &st) != 0 || S_ISDIR(st.st_mode) || (st.st_mode & 0111) == 0) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
			fprintf(stderr, "squire mmap proof debug: executable file stat reject path=%s errno=%d\n", path, errno);
		}
		return 0;
	}
	char mode[32];
	if (!mode_string(st.st_mode, mode)) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
			fprintf(stderr, "squire mmap proof debug: executable file mode reject path=%s\n", path);
		}
		return 0;
	}
	char stat_signal[256];
	if (!file_stat_signal(&st, mode, stat_signal, sizeof(stat_signal))) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
			fprintf(stderr, "squire mmap proof debug: executable file stat-signal reject path=%s\n", path);
		}
		return 0;
	}
	sha256_hex_str(path, sig->path_hash);
	char env_key[128], path_hash_key[160], file_hash_key[160], stat_signal_key[160];
	env_key_for_tool(name, env_key);
	snprintf(path_hash_key, sizeof(path_hash_key), "%s_PATH_HASH", env_key);
	snprintf(file_hash_key, sizeof(file_hash_key), "%s_FILE_HASH", env_key);
	snprintf(stat_signal_key, sizeof(stat_signal_key), "%s_STAT_SIGNAL", env_key);
	const char *cached_path_hash = getenv(path_hash_key);
	const char *cached_file_hash = getenv(file_hash_key);
	const char *cached_stat_signal = getenv(stat_signal_key);
	if (cached_path_hash != NULL && cached_file_hash != NULL && cached_stat_signal != NULL &&
	    strcmp(cached_path_hash, sig->path_hash) == 0 &&
	    strcmp(cached_stat_signal, stat_signal) == 0 &&
	    strlen(cached_file_hash) == HASH_HEX - 1) {
		snprintf(sig->file_hash, HASH_HEX, "%s", cached_file_hash);
		return 1;
	}
	char content_hash[HASH_HEX];
	if (!read_executable_hash(path, content_hash)) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
			fprintf(stderr, "squire mmap proof debug: executable file hash reject path=%s errno=%d\n", path, errno);
		}
		return 0;
	}
	char signal[PATH_BUF + HASH_HEX + 256];
	snprintf(signal, sizeof(signal), "%s|%s|%s", base_name(path), content_hash, stat_signal);
	sha256_hex_str(signal, sig->file_hash);
	return 1;
}

static int deterministic_version_env_hash(char out[HASH_HEX]) {
	static const char *keys[] = {
		"GOROOT", "GOTOOLCHAIN", "GOENV",
		"NODE_OPTIONS", "NVM_BIN", "NVM_DIR",
		"PYENV_VERSION", "VIRTUAL_ENV", "CONDA_PREFIX",
		"CARGO_HOME", "RUSTUP_HOME", "RUSTUP_TOOLCHAIN",
	};
	byte_buf b = {0};
	for (size_t i = 0; i < sizeof(keys) / sizeof(keys[0]); i++) {
		if (i > 0 && !bytes_append_byte(&b, '\n')) {
			bytes_free(&b);
			return 0;
		}
		const char *value = getenv(keys[i]);
		if (value == NULL) {
			value = "";
		}
		char h[HASH_HEX];
		sha256_hex_str(value, h);
		if (!bytes_append_str(&b, keys[i]) || !bytes_append_byte(&b, '=') || !bytes_append_str(&b, h)) {
			bytes_free(&b);
			return 0;
		}
	}
	sha256_hex_buf(&b, out);
	bytes_free(&b);
	return 1;
}

static int hash_selected_environment(const char **keys, size_t key_count, char out[HASH_HEX]) {
	byte_buf b = {0};
	for (size_t i = 0; i < key_count; i++) {
		if (i > 0 && !bytes_append_byte(&b, '\n')) {
			bytes_free(&b);
			return 0;
		}
		const char *value = getenv(keys[i]);
		if (value == NULL) {
			value = "";
		}
		char h[HASH_HEX];
		sha256_hex_str(value, h);
		if (!bytes_append_str(&b, keys[i]) || !bytes_append_byte(&b, '=') || !bytes_append_str(&b, h)) {
			bytes_free(&b);
			return 0;
		}
	}
	sha256_hex_buf(&b, out);
	bytes_free(&b);
	return 1;
}

static int deterministic_static_env_hash(char out[HASH_HEX]) {
	static const char *keys[] = {
		"USER", "LOGNAME", "HOME", "SHELL", "HOSTNAME", "LANG", "LC_ALL", "TZ",
	};
	return hash_selected_environment(keys, sizeof(keys) / sizeof(keys[0]), out);
}

static int file_command_env_hash(char out[HASH_HEX]) {
	static const char *keys[] = {
		"LC_ALL", "LC_CTYPE", "LANG", "MAGIC",
	};
	return hash_selected_environment(keys, sizeof(keys) / sizeof(keys[0]), out);
}

static int sensitive_env_name(const char *name) {
	if (name == NULL || name[0] == '\0') {
		return 1;
	}
	char upper[160];
	size_t n = strlen(name);
	if (n >= sizeof(upper)) {
		return 1;
	}
	for (size_t i = 0; i <= n; i++) {
		upper[i] = (char)toupper((unsigned char)name[i]);
	}
	static const char *markers[] = {
		"TOKEN", "SECRET", "PASSWORD", "PASSWD", "AUTH", "CREDENTIAL", "COOKIE", "BEARER",
		"PRIVATE", "API_KEY", "APIKEY", "ACCESS_KEY", "REFRESH_TOKEN", "SESSION_TOKEN",
	};
	for (size_t i = 0; i < sizeof(markers) / sizeof(markers[0]); i++) {
		if (strstr(upper, markers[i]) != NULL) {
			return 1;
		}
	}
	return 0;
}

static int safe_printenv_name(const char *name) {
	if (name == NULL || name[0] == '\0' || strlen(name) > 128 || sensitive_env_name(name)) {
		return 0;
	}
	for (size_t i = 0; name[i] != '\0'; i++) {
		unsigned char c = (unsigned char)name[i];
		if (i == 0 && isdigit(c)) {
			return 0;
		}
		if (!(isalnum(c) || c == '_')) {
			return 0;
		}
	}
	return 1;
}

static int compare_gids(const void *a, const void *b) {
	gid_t ia = *(const gid_t *)a;
	gid_t ib = *(const gid_t *)b;
	return (ia > ib) - (ia < ib);
}

static int process_identity_signal(char out[HASH_HEX]) {
	int group_count = getgroups(0, NULL);
	if (group_count < 0 || group_count > 4096) {
		group_count = 0;
	}
	gid_t *groups = NULL;
	if (group_count > 0) {
		groups = (gid_t *)calloc((size_t)group_count, sizeof(gid_t));
		if (groups == NULL) {
			return 0;
		}
		if (getgroups(group_count, groups) < 0) {
			free(groups);
			groups = NULL;
			group_count = 0;
		}
	}
	if (groups != NULL && group_count > 1) {
		qsort(groups, (size_t)group_count, sizeof(gid_t), compare_gids);
	}
	byte_buf b = {0};
	char line[128];
	snprintf(line, sizeof(line), "uid=%lld", (long long)getuid());
	int ok = bytes_append_str(&b, line) && bytes_append_byte(&b, '\n');
	snprintf(line, sizeof(line), "euid=%lld", (long long)geteuid());
	ok = ok && bytes_append_str(&b, line) && bytes_append_byte(&b, '\n');
	snprintf(line, sizeof(line), "gid=%lld", (long long)getgid());
	ok = ok && bytes_append_str(&b, line) && bytes_append_byte(&b, '\n');
	snprintf(line, sizeof(line), "egid=%lld", (long long)getegid());
	ok = ok && bytes_append_str(&b, line);
	for (int i = 0; ok && i < group_count; i++) {
		snprintf(line, sizeof(line), "\ngroup=%lld", (long long)groups[i]);
		ok = bytes_append_str(&b, line);
	}
	if (ok) {
		sha256_hex_buf(&b, out);
	}
	bytes_free(&b);
	free(groups);
	return ok;
}

static void file_hash_or_missing(const char *path, char out[HASH_HEX + 16]) {
	char h[HASH_HEX];
	if (read_file_hash(path, h, NULL, NULL)) {
		snprintf(out, HASH_HEX + 16, "%s", h);
		return;
	}
	char path_hash[HASH_HEX];
	sha256_hex_str(path, path_hash);
	snprintf(out, HASH_HEX + 16, "missing:%s", path_hash);
}

static int list_add_path_hash_part(string_list *parts, const char *left, const char *right) {
	byte_buf b = {0};
	int ok = bytes_append_str(&b, left) && bytes_append_byte(&b, 0) && bytes_append_str(&b, right);
	if (ok) {
		ok = list_add_bytes(parts, b.data, b.len);
	}
	bytes_free(&b);
	return ok;
}

static int list_add_env_hash_part(string_list *parts, const char *key) {
	const char *value = getenv(key);
	char h[HASH_HEX];
	sha256_hex_str(value == NULL ? "" : value, h);
	char label[128];
	if (snprintf(label, sizeof(label), "env:%s", key) >= (int)sizeof(label)) {
		return 0;
	}
	return list_add_path_hash_part(parts, label, h);
}

static int list_add_config_file_part(string_list *parts, const char *label, const char *path) {
	if (path == NULL || path[0] == '\0') {
		return 1;
	}
	char left[PATH_BUF + 96], fp[HASH_HEX + 16];
	if (snprintf(left, sizeof(left), "config:%s:%s", label, path) >= (int)sizeof(left)) {
		return 0;
	}
	file_hash_or_missing(path, fp);
	return list_add_path_hash_part(parts, left, fp);
}

static char *trim_ascii_ws(char *s) {
	while (*s == ' ' || *s == '\t' || *s == '\r' || *s == '\n') {
		s++;
	}
	char *end = s + strlen(s);
	while (end > s && (end[-1] == ' ' || end[-1] == '\t' || end[-1] == '\r' || end[-1] == '\n')) {
		*--end = '\0';
	}
	return s;
}

static int git_config_dir(const char *path, char out[PATH_BUF]) {
	snprintf(out, PATH_BUF, "%s", path);
	char *slash = strrchr(out, '/');
	if (slash == NULL) {
		snprintf(out, PATH_BUF, ".");
		return 1;
	}
	if (slash == out) {
		out[1] = '\0';
		return 1;
	}
	*slash = '\0';
	return 1;
}

static int resolve_git_config_include_path(const char *raw, const char *config_path, char out[PATH_BUF]) {
	if (raw == NULL || raw[0] == '\0') {
		return 0;
	}
	char value[PATH_BUF];
	snprintf(value, sizeof(value), "%s", raw);
	char *trimmed = trim_ascii_ws(value);
	size_t len = strlen(trimmed);
	if (len >= 2 && ((trimmed[0] == '"' && trimmed[len - 1] == '"') || (trimmed[0] == '\'' && trimmed[len - 1] == '\''))) {
		trimmed[len - 1] = '\0';
		trimmed++;
	}
	if (trimmed[0] == '\0') {
		return 0;
	}
	if (strcmp(trimmed, "~") == 0) {
		const char *home = getenv("HOME");
		if (home == NULL || home[0] == '\0') {
			return 0;
		}
		return snprintf(out, PATH_BUF, "%s", home) > 0;
	}
	if (strncmp(trimmed, "~/", 2) == 0) {
		const char *home = getenv("HOME");
		if (home == NULL || home[0] == '\0') {
			return 0;
		}
		return join_path(out, PATH_BUF, home, trimmed + 2);
	}
	if (trimmed[0] == '/') {
		return snprintf(out, PATH_BUF, "%s", trimmed) > 0 && strlen(out) < PATH_BUF;
	}
	char dir[PATH_BUF];
	if (!git_config_dir(config_path, dir)) {
		return 0;
	}
	return join_path(out, PATH_BUF, dir, trimmed);
}

static int add_git_config_include_fingerprints(string_list *parts, const char *config_path, int depth, char seen[][PATH_BUF], int *seen_count) {
	if (config_path == NULL || config_path[0] == '\0' || depth > 8 || *seen_count >= 64) {
		return 1;
	}
	for (int i = 0; i < *seen_count; i++) {
		if (strcmp(seen[i], config_path) == 0) {
			return 1;
		}
	}
	snprintf(seen[*seen_count], PATH_BUF, "%s", config_path);
	(*seen_count)++;

	char hash[HASH_HEX];
	unsigned char *content = NULL;
	size_t content_len = 0;
	if (!read_file_hash(config_path, hash, &content, &content_len)) {
		return 1;
	}
	char *text = (char *)malloc(content_len + 1);
	if (text == NULL) {
		free(content);
		return 0;
	}
	if (content_len > 0) {
		memcpy(text, content, content_len);
	}
	text[content_len] = '\0';
	free(content);

	char section[128] = "";
	char *save = NULL;
	for (char *line = strtok_r(text, "\n", &save); line != NULL; line = strtok_r(NULL, "\n", &save)) {
		char *p = trim_ascii_ws(line);
		if (*p == '\0' || *p == '#' || *p == ';') {
			continue;
		}
		if (*p == '[') {
			char *end = strchr(p, ']');
			if (end == NULL) {
				section[0] = '\0';
				continue;
			}
			*end = '\0';
			snprintf(section, sizeof(section), "%s", trim_ascii_ws(p + 1));
			for (char *q = section; *q != '\0'; q++) {
				*q = (char)tolower((unsigned char)*q);
			}
			continue;
		}
		if (strcmp(section, "include") != 0 && strncmp(section, "includeif ", 10) != 0) {
			continue;
		}
		char *eq = strchr(p, '=');
		if (eq == NULL) {
			continue;
		}
		*eq = '\0';
		char *key = trim_ascii_ws(p);
		if (strcasecmp(key, "path") != 0) {
			continue;
		}
		char include_path[PATH_BUF], label[PATH_BUF + 32], fp[HASH_HEX + 16];
		if (!resolve_git_config_include_path(eq + 1, config_path, include_path)) {
			continue;
		}
		file_hash_or_missing(include_path, fp);
		if (snprintf(label, sizeof(label), "config-include:%s", include_path) <= 0 ||
		    !list_add_path_hash_part(parts, label, fp)) {
			free(text);
			return 0;
		}
		if (!add_git_config_include_fingerprints(parts, include_path, depth + 1, seen, seen_count)) {
			free(text);
			return 0;
		}
	}
	free(text);
	return 1;
}

static int list_add_git_config_file_part(string_list *parts, const char *label, const char *path) {
	if (path == NULL || path[0] == '\0') {
		return 1;
	}
	int ok;
	if (label == NULL || label[0] == '\0') {
		char fp[HASH_HEX + 16];
		file_hash_or_missing(path, fp);
		ok = list_add_path_hash_part(parts, path, fp);
	} else {
		ok = list_add_config_file_part(parts, label, path);
	}
	if (!ok) {
		return 0;
	}
	char seen[64][PATH_BUF];
	int seen_count = 0;
	return add_git_config_include_fingerprints(parts, path, 0, seen, &seen_count);
}

static int add_git_config_core_path_fingerprints_from_file(string_list *parts, const char *config_path, const char *key, const char *label, int depth, char seen[][PATH_BUF], int *seen_count) {
	if (config_path == NULL || config_path[0] == '\0' || depth > 8 || *seen_count >= 64) {
		return 1;
	}
	for (int i = 0; i < *seen_count; i++) {
		if (strcmp(seen[i], config_path) == 0) {
			return 1;
		}
	}
	snprintf(seen[*seen_count], PATH_BUF, "%s", config_path);
	(*seen_count)++;

	char hash[HASH_HEX];
	unsigned char *content = NULL;
	size_t content_len = 0;
	if (!read_file_hash(config_path, hash, &content, &content_len)) {
		return 1;
	}
	char *text = (char *)malloc(content_len + 1);
	if (text == NULL) {
		free(content);
		return 0;
	}
	if (content_len > 0) {
		memcpy(text, content, content_len);
	}
	text[content_len] = '\0';
	free(content);

	char section[128] = "";
	char *save = NULL;
	for (char *line = strtok_r(text, "\n", &save); line != NULL; line = strtok_r(NULL, "\n", &save)) {
		char *p = trim_ascii_ws(line);
		if (*p == '\0' || *p == '#' || *p == ';') {
			continue;
		}
		if (*p == '[') {
			char *end = strchr(p, ']');
			if (end == NULL) {
				section[0] = '\0';
				continue;
			}
			*end = '\0';
			snprintf(section, sizeof(section), "%s", trim_ascii_ws(p + 1));
			for (char *q = section; *q != '\0'; q++) {
				*q = (char)tolower((unsigned char)*q);
			}
			continue;
		}
		char *eq = strchr(p, '=');
		if (eq == NULL) {
			continue;
		}
		*eq = '\0';
		char *parsed_key = trim_ascii_ws(p);
		char *value = trim_ascii_ws(eq + 1);
		if ((strcmp(section, "include") == 0 || strncmp(section, "includeif ", 10) == 0) && strcasecmp(parsed_key, "path") == 0) {
			char include_path[PATH_BUF];
			if (resolve_git_config_include_path(value, config_path, include_path) &&
			    !add_git_config_core_path_fingerprints_from_file(parts, include_path, key, label, depth + 1, seen, seen_count)) {
				free(text);
				return 0;
			}
			continue;
		}
		if (strcmp(section, "core") != 0 || strcasecmp(parsed_key, key) != 0) {
			continue;
		}
		char target[PATH_BUF], part_label[PATH_BUF + 64], fp[HASH_HEX + 16];
		if (!resolve_git_config_include_path(value, config_path, target)) {
			continue;
		}
		file_hash_or_missing(target, fp);
		if (snprintf(part_label, sizeof(part_label), "%s:%s", label, target) <= 0 ||
		    !list_add_path_hash_part(parts, part_label, fp)) {
			free(text);
			return 0;
		}
	}
	free(text);
	return 1;
}

static int add_configured_git_core_path_fingerprints(string_list *parts, const char *git_dir, const char *key, const char *label) {
	char seen[64][PATH_BUF];
	int seen_count = 0;
	char path[PATH_BUF];
	if (git_dir != NULL && git_dir[0] != '\0') {
		if (!join_path(path, sizeof(path), git_dir, "config") ||
		    !add_git_config_core_path_fingerprints_from_file(parts, path, key, label, 0, seen, &seen_count)) {
			return 0;
		}
	}
	const char *global = getenv("GIT_CONFIG_GLOBAL");
	const char *home = getenv("HOME");
	const char *xdg = getenv("XDG_CONFIG_HOME");
	if (global != NULL && global[0] != '\0') {
		if (!add_git_config_core_path_fingerprints_from_file(parts, global, key, label, 0, seen, &seen_count)) {
			return 0;
		}
	} else if (home != NULL && home[0] != '\0') {
		if (!join_path(path, sizeof(path), home, ".gitconfig") ||
		    !add_git_config_core_path_fingerprints_from_file(parts, path, key, label, 0, seen, &seen_count)) {
			return 0;
		}
		if (xdg != NULL && xdg[0] != '\0') {
			if (!join_path(path, sizeof(path), xdg, "git/config") ||
			    !add_git_config_core_path_fingerprints_from_file(parts, path, key, label, 0, seen, &seen_count)) {
				return 0;
			}
		} else {
			if (!join_path(path, sizeof(path), home, ".config/git/config") ||
			    !add_git_config_core_path_fingerprints_from_file(parts, path, key, label, 0, seen, &seen_count)) {
				return 0;
			}
		}
	}
	const char *system = getenv("GIT_CONFIG_SYSTEM");
	if (system != NULL && system[0] != '\0') {
		if (!add_git_config_core_path_fingerprints_from_file(parts, system, key, label, 0, seen, &seen_count)) {
			return 0;
		}
	} else if (getenv("GIT_CONFIG_NOSYSTEM") == NULL) {
		if (!add_git_config_core_path_fingerprints_from_file(parts, "/etc/gitconfig", key, label, 0, seen, &seen_count) ||
		    !add_git_config_core_path_fingerprints_from_file(parts, "/usr/local/etc/gitconfig", key, label, 0, seen, &seen_count) ||
		    !add_git_config_core_path_fingerprints_from_file(parts, "/opt/homebrew/etc/gitconfig", key, label, 0, seen, &seen_count)) {
			return 0;
		}
	}
	return 1;
}

static int add_external_git_config_fingerprints(string_list *parts) {
	static const char *keys[] = {
		"GIT_CONFIG_NOSYSTEM",
		"GIT_CONFIG_SYSTEM",
		"GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_PARAMETERS",
		"HOME",
		"XDG_CONFIG_HOME",
	};
	for (size_t i = 0; i < sizeof(keys) / sizeof(keys[0]); i++) {
		if (!list_add_env_hash_part(parts, keys[i])) {
			return 0;
		}
	}
	const char *count_env = getenv("GIT_CONFIG_COUNT");
	if (count_env != NULL) {
		int count = atoi(count_env);
		if (count > 0 && count < 128) {
			for (int i = 0; i < count; i++) {
				char key[64], value[64];
				snprintf(key, sizeof(key), "GIT_CONFIG_KEY_%d", i);
				snprintf(value, sizeof(value), "GIT_CONFIG_VALUE_%d", i);
				if (!list_add_env_hash_part(parts, key) || !list_add_env_hash_part(parts, value)) {
					return 0;
				}
			}
		}
	}
	const char *global = getenv("GIT_CONFIG_GLOBAL");
	const char *home = getenv("HOME");
	const char *xdg = getenv("XDG_CONFIG_HOME");
	char path[PATH_BUF];
	if (global != NULL && global[0] != '\0') {
		if (!list_add_git_config_file_part(parts, "global-env", global)) {
			return 0;
		}
	} else if (home != NULL && home[0] != '\0') {
		if (!join_path(path, sizeof(path), home, ".gitconfig") ||
		    !list_add_git_config_file_part(parts, "global-home", path)) {
			return 0;
		}
		if (xdg != NULL && xdg[0] != '\0') {
			if (!join_path(path, sizeof(path), xdg, "git/config") ||
			    !list_add_git_config_file_part(parts, "global-xdg", path)) {
				return 0;
			}
		} else {
			if (!join_path(path, sizeof(path), home, ".config/git/config") ||
			    !list_add_git_config_file_part(parts, "global-xdg", path)) {
				return 0;
			}
		}
	}
	const char *system = getenv("GIT_CONFIG_SYSTEM");
	if (system != NULL && system[0] != '\0') {
		if (!list_add_git_config_file_part(parts, "system-env", system)) {
			return 0;
		}
	} else if (getenv("GIT_CONFIG_NOSYSTEM") == NULL) {
		if (!list_add_git_config_file_part(parts, "system", "/etc/gitconfig") ||
		    !list_add_git_config_file_part(parts, "system", "/usr/local/etc/gitconfig") ||
		    !list_add_git_config_file_part(parts, "system", "/opt/homebrew/etc/gitconfig")) {
			return 0;
		}
	}
	return 1;
}

static int git_config_summary_fingerprint(const char *repo_root, const char *git_dir, char out[HASH_HEX]) {
	(void)repo_root;
	string_list parts = {0};
	char path[PATH_BUF], fp[HASH_HEX + 16];
	if (!join_path(path, sizeof(path), git_dir, "config")) {
		return 0;
	}
	if (!list_add_git_config_file_part(&parts, "", path)) {
		list_free(&parts);
		return 0;
	}
	if (!join_path(path, sizeof(path), git_dir, "info/sparse-checkout")) {
		list_free(&parts);
		return 0;
	}
	file_hash_or_missing(path, fp);
	if (!list_add_path_hash_part(&parts, path, fp)) {
		list_free(&parts);
		return 0;
	}
	if (!add_external_git_config_fingerprints(&parts)) {
		list_free(&parts);
		return 0;
	}
	int ok = hash_joined_lines(&parts, out);
	list_free(&parts);
	return ok;
}

static int git_attribute_fingerprint(const char *repo_root, const char *git_dir, char out[HASH_HEX]) {
	string_list parts = {0};
	char path[PATH_BUF], fp[HASH_HEX + 16];
	if (!join_path(path, sizeof(path), repo_root, ".gitattributes")) {
		return 0;
	}
	file_hash_or_missing(path, fp);
	if (!list_add_path_hash_part(&parts, path, fp)) {
		list_free(&parts);
		return 0;
	}
	if (!join_path(path, sizeof(path), git_dir, "info/attributes")) {
		list_free(&parts);
		return 0;
	}
	file_hash_or_missing(path, fp);
	if (!list_add_path_hash_part(&parts, path, fp)) {
		list_free(&parts);
		return 0;
	}
	if (!add_configured_git_core_path_fingerprints(&parts, git_dir, "attributesfile", "attributes:core-attributes")) {
		list_free(&parts);
		return 0;
	}
	const char *home = getenv("HOME");
	const char *xdg = getenv("XDG_CONFIG_HOME");
	if (home != NULL && home[0] != '\0') {
		char env_part[HASH_HEX];
		sha256_hex_str(home, env_part);
		if (!list_add_path_hash_part(&parts, "env:HOME", env_part)) {
			list_free(&parts);
			return 0;
		}
	}
	if (xdg != NULL && xdg[0] != '\0') {
		char attr_path[PATH_BUF], label[PATH_BUF + 32], attr_fp[HASH_HEX + 16], env_part[HASH_HEX];
		sha256_hex_str(xdg, env_part);
		if (!list_add_path_hash_part(&parts, "env:XDG_CONFIG_HOME", env_part)) {
			list_free(&parts);
			return 0;
		}
		if (!join_path(attr_path, sizeof(attr_path), xdg, "git/attributes")) {
			list_free(&parts);
			return 0;
		}
		file_hash_or_missing(attr_path, attr_fp);
		if (snprintf(label, sizeof(label), "attributes:global-xdg:%s", attr_path) <= 0 ||
		    !list_add_path_hash_part(&parts, label, attr_fp)) {
			list_free(&parts);
			return 0;
		}
	} else if (home != NULL && home[0] != '\0') {
		char attr_path[PATH_BUF], label[PATH_BUF + 32], attr_fp[HASH_HEX + 16];
		if (!join_path(attr_path, sizeof(attr_path), home, ".config/git/attributes")) {
			list_free(&parts);
			return 0;
		}
		file_hash_or_missing(attr_path, attr_fp);
		if (snprintf(label, sizeof(label), "attributes:global-xdg:%s", attr_path) <= 0 ||
		    !list_add_path_hash_part(&parts, label, attr_fp)) {
			list_free(&parts);
			return 0;
		}
	}
	int ok = hash_joined_lines_ordered(&parts, out);
	list_free(&parts);
	return ok;
}

static long long stat_mtime_nano(const struct stat *st) {
#if defined(__APPLE__)
	return (long long)st->st_mtimespec.tv_sec * 1000000000LL + (long long)st->st_mtimespec.tv_nsec;
#elif defined(__linux__)
	return (long long)st->st_mtim.tv_sec * 1000000000LL + (long long)st->st_mtim.tv_nsec;
#else
	return (long long)st->st_mtime * 1000000000LL;
#endif
}

static int file_stat_signal(const struct stat *st, const char *mode, char *out, size_t cap) {
#if defined(__APPLE__)
	return snprintf(out, cap, "%lld|%lld|%s|dev=%llu|ino=%llu|ctime=%lld.%ld",
	                (long long)st->st_size,
	                stat_mtime_nano(st),
	                mode,
	                (unsigned long long)st->st_dev,
	                (unsigned long long)st->st_ino,
	                (long long)st->st_ctimespec.tv_sec,
	                (long)st->st_ctimespec.tv_nsec) > 0;
#elif defined(__linux__)
	return snprintf(out, cap, "%lld|%lld|%s|dev=%llu|ino=%llu|ctime=%lld.%ld",
	                (long long)st->st_size,
	                stat_mtime_nano(st),
	                mode,
	                (unsigned long long)st->st_dev,
	                (unsigned long long)st->st_ino,
	                (long long)st->st_ctim.tv_sec,
	                (long)st->st_ctim.tv_nsec) > 0;
#else
	return snprintf(out, cap, "%lld|%lld|%s|change:unsupported",
	                (long long)st->st_size,
	                stat_mtime_nano(st),
	                mode) > 0;
#endif
}

static int relative_from_root(const char *root, const char *path, char rel[PATH_BUF]) {
	size_t root_len = strlen(root);
	if (strcmp(root, path) == 0) {
		rel[0] = '\0';
		return 1;
	}
	if (strncmp(path, root, root_len) != 0 || path[root_len] != '/') {
		return 0;
	}
	snprintf(rel, PATH_BUF, "%s", path + root_len + 1);
	return 1;
}

static int collect_named_file_fingerprints(const char *root, const char *dir, const char *name, string_list *parts) {
	DIR *d = opendir(dir);
	if (d == NULL) {
		return 1;
	}
	struct dirent *ent;
	while ((ent = readdir(d)) != NULL) {
		if (strcmp(ent->d_name, ".") == 0 || strcmp(ent->d_name, "..") == 0) {
			continue;
		}
		char path[PATH_BUF];
		if (!join_path(path, sizeof(path), dir, ent->d_name)) {
			closedir(d);
			return 0;
		}
		struct stat st;
		if (lstat(path, &st) != 0) {
			continue;
		}
		if (S_ISDIR(st.st_mode)) {
			if (strcmp(ent->d_name, ".git") == 0 || strcmp(ent->d_name, ".squire") == 0) {
				continue;
			}
			if (!collect_named_file_fingerprints(root, path, name, parts)) {
				closedir(d);
				return 0;
			}
			continue;
		}
		if (strcmp(ent->d_name, name) != 0) {
			continue;
		}
		char rel[PATH_BUF], fp[HASH_HEX + 16];
		if (!relative_from_root(root, path, rel)) {
			continue;
		}
		file_hash_or_missing(path, fp);
		if (!list_add_path_hash_part(parts, rel, fp)) {
			closedir(d);
			return 0;
		}
	}
	closedir(d);
	return 1;
}

static int workspace_ignore_fingerprint(const char *repo_root, const char *git_dir, char out[HASH_HEX]) {
	string_list parts = {0};
	static const char *names[] = {".gitignore", ".ignore", ".rgignore"};
	for (size_t i = 0; i < sizeof(names) / sizeof(names[0]); i++) {
		if (!collect_named_file_fingerprints(repo_root, repo_root, names[i], &parts)) {
			list_free(&parts);
			return 0;
		}
	}
	if (git_dir != NULL && git_dir[0] != '\0') {
		char path[PATH_BUF], fp[HASH_HEX + 16];
		if (!join_path(path, sizeof(path), git_dir, "info/exclude")) {
			list_free(&parts);
			return 0;
		}
		file_hash_or_missing(path, fp);
		if (!list_add_path_hash_part(&parts, path, fp)) {
			list_free(&parts);
			return 0;
		}
	}
	if (git_dir != NULL && git_dir[0] != '\0' &&
	    !add_configured_git_core_path_fingerprints(&parts, git_dir, "excludesfile", "ignore:core-excludes")) {
		list_free(&parts);
		return 0;
	}
	const char *home = getenv("HOME");
	const char *xdg = getenv("XDG_CONFIG_HOME");
	if (home != NULL && home[0] != '\0') {
		char env_part[HASH_HEX];
		sha256_hex_str(home, env_part);
		if (!list_add_path_hash_part(&parts, "env:HOME", env_part)) {
			list_free(&parts);
			return 0;
		}
	}
	if (xdg != NULL && xdg[0] != '\0') {
		char path[PATH_BUF], label[PATH_BUF + 32], fp[HASH_HEX + 16], env_part[HASH_HEX];
		sha256_hex_str(xdg, env_part);
		if (!list_add_path_hash_part(&parts, "env:XDG_CONFIG_HOME", env_part)) {
			list_free(&parts);
			return 0;
		}
		if (!join_path(path, sizeof(path), xdg, "git/ignore")) {
			list_free(&parts);
			return 0;
		}
		file_hash_or_missing(path, fp);
		if (snprintf(label, sizeof(label), "ignore:global-xdg:%s", path) <= 0 ||
		    !list_add_path_hash_part(&parts, label, fp)) {
			list_free(&parts);
			return 0;
		}
	} else if (home != NULL && home[0] != '\0') {
		char path[PATH_BUF], label[PATH_BUF + 32], fp[HASH_HEX + 16];
		if (!join_path(path, sizeof(path), home, ".config/git/ignore")) {
			list_free(&parts);
			return 0;
		}
		file_hash_or_missing(path, fp);
		if (snprintf(label, sizeof(label), "ignore:global-xdg:%s", path) <= 0 ||
		    !list_add_path_hash_part(&parts, label, fp)) {
			list_free(&parts);
			return 0;
		}
	}
	int ok = hash_joined_lines(&parts, out);
	list_free(&parts);
	return ok;
}

typedef struct {
	const char *root;
	int need_content;
	int max_content_files;
	int content_count;
	int complete;
	string_list tree;
	string_list content;
} workspace_epoch_builder;

static int add_workspace_part(string_list *list, const char *rel, const char *mode, long long size) {
	char size_s[64];
	snprintf(size_s, sizeof(size_s), "%lld", size);
	byte_buf b = {0};
	int ok = bytes_append_str(&b, rel) &&
	         bytes_append_byte(&b, 0) &&
	         bytes_append_str(&b, mode) &&
	         bytes_append_byte(&b, 0) &&
	         bytes_append_str(&b, size_s);
	if (ok) {
		ok = list_add_bytes(list, b.data, b.len);
	}
	bytes_free(&b);
	return ok;
}

static int add_content_part(string_list *list, const char *rel, const char *hash) {
	byte_buf b = {0};
	int ok = bytes_append_str(&b, rel) && bytes_append_byte(&b, 0) && bytes_append_str(&b, hash);
	if (ok) {
		ok = list_add_bytes(list, b.data, b.len);
	}
	bytes_free(&b);
	return ok;
}

static int collect_workspace_epochs(workspace_epoch_builder *b, const char *dir) {
	DIR *d = opendir(dir);
	if (d == NULL) {
		b->complete = 0;
		return 1;
	}
	struct dirent *ent;
	while ((ent = readdir(d)) != NULL) {
		if (strcmp(ent->d_name, ".") == 0 || strcmp(ent->d_name, "..") == 0) {
			continue;
		}
		char path[PATH_BUF];
		if (!join_path(path, sizeof(path), dir, ent->d_name)) {
			b->complete = 0;
			continue;
		}
		struct stat st;
		if (lstat(path, &st) != 0) {
			b->complete = 0;
			continue;
		}
		if (S_ISDIR(st.st_mode) && (strcmp(ent->d_name, ".git") == 0 || strcmp(ent->d_name, ".squire") == 0)) {
			continue;
		}
		char rel[PATH_BUF], mode[32];
		if (!relative_from_root(b->root, path, rel) || !mode_string(st.st_mode, mode)) {
			b->complete = 0;
			continue;
		}
		if (!add_workspace_part(&b->tree, rel, mode, (long long)st.st_size)) {
			closedir(d);
			return 0;
		}
		if (S_ISDIR(st.st_mode)) {
			if (!collect_workspace_epochs(b, path)) {
				closedir(d);
				return 0;
			}
			continue;
		}
		if (!S_ISREG(st.st_mode) || !b->need_content) {
			continue;
		}
		b->content_count++;
		if (b->max_content_files > 0 && b->content_count > b->max_content_files) {
			b->complete = 0;
			continue;
		}
		char fp[HASH_HEX];
		if (!read_file_hash(path, fp, NULL, NULL)) {
			b->complete = 0;
			continue;
		}
		if (!add_content_part(&b->content, rel, fp)) {
			closedir(d);
			return 0;
		}
	}
	closedir(d);
	return 1;
}

static int exact_workspace_epochs(const char *root, int max_content_files, int need_content, char tree[HASH_HEX], char content[HASH_HEX]) {
	workspace_epoch_builder b = {0};
	b.root = root;
	b.need_content = need_content;
	b.max_content_files = max_content_files;
	b.complete = 1;
	if (!collect_workspace_epochs(&b, root)) {
		list_free(&b.tree);
		list_free(&b.content);
		return 0;
	}
	int ok = b.complete && hash_joined_lines(&b.tree, tree) && hash_joined_lines(&b.content, content);
	list_free(&b.tree);
	list_free(&b.content);
	return ok;
}

static int safe_git_config_override(const char *value) {
	const char *eq = strchr(value, '=');
	if (eq == NULL) {
		return 0;
	}
	size_t n = (size_t)(eq - value);
	return (n == strlen("core.hookspath") && strncasecmp(value, "core.hookspath", n) == 0) ||
	       (n == strlen("core.fsmonitor") && strncasecmp(value, "core.fsmonitor", n) == 0);
}

static int normalize_invocation_at_cwd(const char *cwd, int argc, char **argv, policy_invocation *out) {
	if (argc <= 0 || argc > MAX_ARGC) {
		return 0;
	}
	if (cwd != NULL && cwd[0] != '\0') {
		snprintf(out->cwd, sizeof(out->cwd), "%s", cwd);
	} else if (getcwd(out->cwd, sizeof(out->cwd)) == NULL) {
		return 0;
	}
	char cwd_real[PATH_BUF];
	if (realpath(out->cwd, cwd_real) != NULL) {
		snprintf(out->cwd, sizeof(out->cwd), "%s", cwd_real);
	}
	out->argc = 0;
	const char *tool = base_name(argv[0]);
	snprintf(out->storage[out->argc], PATH_BUF, "%s", tool);
	out->argv[out->argc] = out->storage[out->argc];
	out->argc++;

	if (strcmp(tool, "git") != 0) {
		for (int i = 1; i < argc; i++) {
			if (out->argc >= MAX_ARGC) {
				return 0;
			}
			snprintf(out->storage[out->argc], PATH_BUF, "%s", argv[i]);
			out->argv[out->argc] = out->storage[out->argc];
			out->argc++;
		}
		return 1;
	}

	if (argc == 2 && (strcmp(argv[1], "--version") == 0 || strcmp(argv[1], "version") == 0)) {
		snprintf(out->storage[out->argc], PATH_BUF, "%s", argv[1]);
		out->argv[out->argc] = out->storage[out->argc];
		out->argc++;
		return 1;
	}

	int i = 1;
	int changed = 0;
	while (i < argc) {
		const char *arg = argv[i];
		if (strcmp(arg, "-C") == 0) {
			if (i + 1 >= argc) {
				return 0;
			}
			char resolved[PATH_BUF];
			if (!absolute_path(argv[i + 1], out->cwd, resolved)) {
				return 0;
			}
			if (realpath(resolved, cwd_real) != NULL) {
				snprintf(out->cwd, sizeof(out->cwd), "%s", cwd_real);
			} else {
				snprintf(out->cwd, sizeof(out->cwd), "%s", resolved);
			}
			i += 2;
			changed = 1;
			continue;
		}
		if (strncmp(arg, "-C", 2) == 0 && strlen(arg) > 2) {
			char resolved[PATH_BUF];
			if (!absolute_path(arg + 2, out->cwd, resolved)) {
				return 0;
			}
			if (realpath(resolved, cwd_real) != NULL) {
				snprintf(out->cwd, sizeof(out->cwd), "%s", cwd_real);
			} else {
				snprintf(out->cwd, sizeof(out->cwd), "%s", resolved);
			}
			i++;
			changed = 1;
			continue;
		}
		if (strcmp(arg, "-c") == 0) {
			if (i + 1 >= argc || !safe_git_config_override(argv[i + 1])) {
				return 0;
			}
			i += 2;
			changed = 1;
			continue;
		}
		if (strncmp(arg, "-c", 2) == 0 && strlen(arg) > 2) {
			if (!safe_git_config_override(arg + 2)) {
				return 0;
			}
			i++;
			changed = 1;
			continue;
		}
		if (arg[0] == '-') {
			return 0;
		}
		break;
	}
	for (; i < argc; i++) {
		if (out->argc >= MAX_ARGC) {
			return 0;
		}
		(void)changed;
		snprintf(out->storage[out->argc], PATH_BUF, "%s", argv[i]);
		out->argv[out->argc] = out->storage[out->argc];
		out->argc++;
	}
	return 1;
}

static int normalize_invocation(int argc, char **argv, policy_invocation *out) {
	return normalize_invocation_at_cwd(NULL, argc, argv, out);
}

static void command_key(policy_invocation *inv, char out[HASH_HEX]) {
	SQUIRE_SHA256_CTX ctx;
	SQUIRE_SHA256_Init(&ctx);
	SQUIRE_SHA256_Update(&ctx, (const unsigned char *)inv->cwd, strlen(inv->cwd));
	unsigned char zero = 0;
	SQUIRE_SHA256_Update(&ctx, &zero, 1);
	for (int i = 0; i < inv->argc; i++) {
		if (i > 0) {
			SQUIRE_SHA256_Update(&ctx, &zero, 1);
		}
		SQUIRE_SHA256_Update(&ctx, (const unsigned char *)inv->argv[i], strlen(inv->argv[i]));
	}
	unsigned char digest[32];
	static const char hex[] = "0123456789abcdef";
	SQUIRE_SHA256_Final(digest, &ctx);
	for (int i = 0; i < 32; i++) {
		out[i * 2] = hex[digest[i] >> 4];
		out[i * 2 + 1] = hex[digest[i] & 0x0f];
	}
	out[64] = '\0';
}

static int is_git_metadata(policy_invocation *inv) {
	if (inv->argc == 3 &&
	    strcmp(inv->argv[0], "git") == 0 &&
	    strcmp(inv->argv[1], "branch") == 0 &&
	    strcmp(inv->argv[2], "--show-current") == 0) {
		return 1;
	}
	if (inv->argc == 3 && strcmp(inv->argv[0], "git") == 0 && strcmp(inv->argv[1], "rev-parse") == 0) {
		return strcmp(inv->argv[2], "HEAD") == 0 ||
		       strcmp(inv->argv[2], "--git-dir") == 0 ||
		       strcmp(inv->argv[2], "--show-toplevel") == 0 ||
		       strcmp(inv->argv[2], "--is-inside-work-tree") == 0;
	}
	return inv->argc == 4 &&
	       strcmp(inv->argv[0], "git") == 0 &&
	       strcmp(inv->argv[1], "rev-parse") == 0 &&
	       strcmp(inv->argv[2], "--abbrev-ref") == 0 &&
	       strcmp(inv->argv[3], "HEAD") == 0;
}

static int git_metadata_epoch(policy_invocation *inv, char epoch[256]) {
	if (!is_git_metadata(inv)) {
		return 0;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF], head[128], branch[PATH_BUF], git_rel[PATH_BUF];
	if (!discover_git_dir(inv->cwd, repo_root, git_dir) || !current_head_and_branch(git_dir, head, branch) || !rel_git_dir(inv->cwd, git_dir, git_rel)) {
		return 0;
	}
	char buf[PATH_BUF * 2];
	char h[HASH_HEX];
	if (inv->argc == 3 && strcmp(inv->argv[2], "HEAD") == 0) {
		snprintf(buf, sizeof(buf), "%s|%s", repo_root, head);
		sha256_hex_str(buf, h);
		snprintf(epoch, 256, "hot-head:%s", h);
		return 1;
	}
	if (inv->argc == 4 && strcmp(inv->argv[2], "--abbrev-ref") == 0) {
		snprintf(buf, sizeof(buf), "%s|%s|%s", repo_root, branch, head);
		sha256_hex_str(buf, h);
		snprintf(epoch, 256, "hot-branch:%s", h);
		return 1;
	}
	if (inv->argc == 3 && strcmp(inv->argv[1], "branch") == 0 && strcmp(inv->argv[2], "--show-current") == 0) {
		snprintf(buf, sizeof(buf), "%s|%s|%s", repo_root, branch, head);
		sha256_hex_str(buf, h);
		snprintf(epoch, 256, "hot-branch:%s", h);
		return 1;
	}
	if (strcmp(inv->argv[2], "--git-dir") == 0) {
		snprintf(buf, sizeof(buf), "%s|%s|%s", repo_root, git_rel, git_dir);
		sha256_hex_str(buf, h);
		snprintf(epoch, 256, "hot-gitdir:%s", h);
		return 1;
	}
	if (strcmp(inv->argv[2], "--show-toplevel") == 0) {
		snprintf(buf, sizeof(buf), "%s|%s", repo_root, git_dir);
		sha256_hex_str(buf, h);
		snprintf(epoch, 256, "hot-repo-root:%s", h);
		return 1;
	}
	if (strcmp(inv->argv[2], "--is-inside-work-tree") == 0) {
		snprintf(buf, sizeof(buf), "%s|%s", repo_root, git_dir);
		sha256_hex_str(buf, h);
		snprintf(epoch, 256, "hot-worktree:%s", h);
		return 1;
	}
	return 0;
}

static int is_git_ls_files(policy_invocation *inv) {
	if (inv->argc == 2 && strcmp(inv->argv[0], "git") == 0 && strcmp(inv->argv[1], "ls-files") == 0) {
		return 1;
	}
	return inv->argc == 3 &&
	       strcmp(inv->argv[0], "git") == 0 &&
	       strcmp(inv->argv[1], "ls-files") == 0 &&
	       safe_relative_inspection_path_arg(inv->argv[2]);
}

static int is_git_status(policy_invocation *inv) {
	return inv->argc == 3 &&
	       strcmp(inv->argv[0], "git") == 0 &&
	       strcmp(inv->argv[1], "status") == 0 &&
	       (strcmp(inv->argv[2], "--short") == 0 || strcmp(inv->argv[2], "--porcelain") == 0);
}

static int safe_relative_inspection_path_arg(const char *path) {
	char rel[PATH_BUF];
	return clean_relative_path(path, rel);
}

static int is_git_read_only_diff(policy_invocation *inv) {
	if (inv->argc < 2 || strcmp(inv->argv[0], "git") != 0 || strcmp(inv->argv[1], "diff") != 0) {
		return 0;
	}
	if (inv->argc == 2) {
		return 1;
	}
	if (inv->argc == 3 && strcmp(inv->argv[2], "--stat") == 0) {
		return 1;
	}
	if (inv->argc >= 4 && strcmp(inv->argv[2], "--") == 0) {
		for (int i = 3; i < inv->argc; i++) {
			if (!safe_relative_inspection_path_arg(inv->argv[i])) {
				return 0;
			}
		}
		return 1;
	}
	return 0;
}

static int append_normalized_epoch_input(byte_buf *b, policy_invocation *inv) {
	return bytes_append_argv_norm(b, inv);
}

static int repo_summary_epoch(policy_invocation *inv, char epoch[256]) {
	if (!is_git_ls_files(inv) && !is_git_status(inv) && !is_git_read_only_diff(inv)) {
		return 0;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF];
	if (!discover_git_dir(inv->cwd, repo_root, git_dir)) {
		return 0;
	}
	executable_signal tool;
	if (!executable_signal_for(inv->cwd, "git", &tool)) {
		return 0;
	}
	char index_path[PATH_BUF], index_fp[HASH_HEX + 16], config_fp[HASH_HEX], tree[HASH_HEX], content[HASH_HEX], input_hash[HASH_HEX];
	if (!join_path(index_path, sizeof(index_path), git_dir, "index")) {
		return 0;
	}
	file_hash_or_missing(index_path, index_fp);
	if (!git_config_summary_fingerprint(repo_root, git_dir, config_fp)) {
		return 0;
	}
	byte_buf b = {0};
	int ok = 0;
	if (is_git_ls_files(inv)) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: ls-files index=%s config=%s tool=%s\n", index_fp, config_fp, tool.file_hash);
		}
		ok = bytes_append_str(&b, repo_root) &&
		     bytes_append_byte(&b, '|') &&
		     append_normalized_epoch_input(&b, inv) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, index_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, config_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, tool.file_hash);
		if (ok) {
			sha256_hex_buf(&b, input_hash);
			snprintf(epoch, 256, "hot-repo-summary:git-ls-files:%s", input_hash);
		}
		bytes_free(&b);
		return ok;
	}
	if (!exact_workspace_epochs(repo_root, 10000, 1, tree, content)) {
		bytes_free(&b);
		return 0;
	}
	if (is_git_status(inv)) {
		char head[128], branch[PATH_BUF], ignore_fp[HASH_HEX];
		if (!current_head_and_branch(git_dir, head, branch) || !workspace_ignore_fingerprint(repo_root, git_dir, ignore_fp)) {
			bytes_free(&b);
			return 0;
		}
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: status head=%s branch=%s index=%s config=%s ignore=%s tree=%s content=%s tool=%s\n", head, branch, index_fp, config_fp, ignore_fp, tree, content, tool.file_hash);
		}
		ok = bytes_append_str(&b, repo_root) &&
		     bytes_append_byte(&b, '|') &&
		     append_normalized_epoch_input(&b, inv) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, head) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, branch) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, index_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, config_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, ignore_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, tree) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, content) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, tool.file_hash);
		if (ok) {
			sha256_hex_buf(&b, input_hash);
			snprintf(epoch, 256, "hot-repo-summary:git-status:%s", input_hash);
		}
		bytes_free(&b);
		return ok;
	}
	if (is_git_read_only_diff(inv)) {
		char attr_fp[HASH_HEX];
		if (!git_attribute_fingerprint(repo_root, git_dir, attr_fp)) {
			bytes_free(&b);
			return 0;
		}
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: diff index=%s config=%s attr=%s tree=%s content=%s tool=%s\n", index_fp, config_fp, attr_fp, tree, content, tool.file_hash);
		}
		ok = bytes_append_str(&b, repo_root) &&
		     bytes_append_byte(&b, '|') &&
		     append_normalized_epoch_input(&b, inv) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, index_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, config_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, attr_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, tree) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, content) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, tool.file_hash);
		if (ok) {
			sha256_hex_buf(&b, input_hash);
			snprintf(epoch, 256, "hot-repo-summary:git-diff:%s", input_hash);
		}
		bytes_free(&b);
		return ok;
	}
	bytes_free(&b);
	return 0;
}

static int is_common_tool_name(const char *name) {
	static const char *tools[] = {"git", "rg", "go", "node", "npm", "pnpm", "yarn", "python", "python3", "pip", "pip3", "cargo", "rustc", "make"};
	for (size_t i = 0; i < sizeof(tools) / sizeof(tools[0]); i++) {
		if (strcmp(name, tools[i]) == 0) {
			return 1;
		}
	}
	return 0;
}

static int is_tool_version_probe(policy_invocation *inv) {
	if (inv->argc != 2) {
		return 0;
	}
	const char *name = inv->argv[0];
	const char *arg = inv->argv[1];
	if (strcmp(name, "git") == 0) {
		return strcmp(arg, "--version") == 0 || strcmp(arg, "version") == 0;
	}
	if (strcmp(name, "go") == 0) {
		return strcmp(arg, "version") == 0;
	}
	if (strcmp(name, "node") == 0 || strcmp(name, "npm") == 0 || strcmp(name, "pnpm") == 0 || strcmp(name, "yarn") == 0) {
		return strcmp(arg, "--version") == 0 || strcmp(arg, "-v") == 0;
	}
	if (strcmp(name, "python") == 0 || strcmp(name, "python3") == 0 || strcmp(name, "pip") == 0 || strcmp(name, "pip3") == 0 ||
	    strcmp(name, "cargo") == 0 || strcmp(name, "rustc") == 0 || strcmp(name, "rg") == 0) {
		return strcmp(arg, "--version") == 0;
	}
	return 0;
}

static int command_path_lookup_target(policy_invocation *inv, const char **target) {
	if (inv->argc == 2 && strcmp(inv->argv[0], "which") == 0) {
		*target = inv->argv[1];
	} else if (inv->argc == 3 && strcmp(inv->argv[0], "command") == 0 && strcmp(inv->argv[1], "-v") == 0) {
		*target = inv->argv[2];
	} else {
		return 0;
	}
	if ((*target)[0] == '\0' || strchr(*target, '/') != NULL || strchr(*target, '\\') != NULL || (*target)[0] == '-') {
		return 0;
	}
	return is_common_tool_name(*target);
}

static int tool_version_epoch(policy_invocation *inv, char epoch[256]) {
	if (!is_tool_version_probe(inv)) {
		return 0;
	}
	executable_signal sig;
	if (!executable_signal_for(inv->cwd, inv->argv[0], &sig)) {
		return 0;
	}
	char path_hash[HASH_HEX], version_hash[HASH_HEX], input_hash[HASH_HEX];
	sha256_hex_str(proof_path_env(), path_hash);
	if (!deterministic_version_env_hash(version_hash)) {
		return 0;
	}
	byte_buf b = {0};
	int ok = bytes_append_str(&b, inv->argv[0]) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.file_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, version_hash);
	if (ok) {
		sha256_hex_buf(&b, input_hash);
		snprintf(epoch, 256, "hot-tool-version:%s", input_hash);
	}
	bytes_free(&b);
	return ok;
}

static int command_path_epoch(policy_invocation *inv, char epoch[256]) {
	const char *target = NULL;
	if (!command_path_lookup_target(inv, &target)) {
		return 0;
	}
	executable_signal which_sig;
	int which_ok = 0;
	if (strcmp(inv->argv[0], "command") == 0) {
		sha256_hex_str("shell-builtin:command", which_sig.path_hash);
		sha256_hex_str("shell-builtin:command-v", which_sig.file_hash);
		which_ok = 1;
	} else {
		which_ok = executable_signal_for(inv->cwd, inv->argv[0], &which_sig);
	}
	executable_signal target_sig;
	if (!which_ok || !executable_signal_for(inv->cwd, target, &target_sig)) {
		return 0;
	}
	char path_hash[HASH_HEX], version_hash[HASH_HEX], input_hash[HASH_HEX];
	sha256_hex_str(proof_path_env(), path_hash);
	if (!deterministic_version_env_hash(version_hash)) {
		return 0;
	}
	byte_buf b = {0};
	int ok = bytes_append_str(&b, target) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, which_sig.path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, which_sig.file_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, target_sig.path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, target_sig.file_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, version_hash);
	if (ok) {
		sha256_hex_buf(&b, input_hash);
		snprintf(epoch, 256, "hot-command-path:%s", input_hash);
	}
	bytes_free(&b);
	return ok;
}

static int is_static_environment_probe(policy_invocation *inv) {
	if (inv->argc < 1 || inv->argc > 2) {
		return 0;
	}
	const char *name = inv->argv[0];
	if (strcmp(name, "whoami") == 0 || strcmp(name, "hostname") == 0 || strcmp(name, "id") == 0) {
		return inv->argc == 1;
	}
	if (strcmp(name, "uname") == 0) {
		if (inv->argc == 1) {
			return 1;
		}
		const char *arg = inv->argv[1];
		return strcmp(arg, "-a") == 0 || strcmp(arg, "-m") == 0 || strcmp(arg, "-n") == 0 ||
		       strcmp(arg, "-r") == 0 || strcmp(arg, "-s") == 0 || strcmp(arg, "-v") == 0;
	}
	return 0;
}

static int static_environment_epoch(policy_invocation *inv, char epoch[256]) {
	if (!is_static_environment_probe(inv)) {
		return 0;
	}
	executable_signal sig;
	if (!executable_signal_for(inv->cwd, inv->argv[0], &sig)) {
		return 0;
	}
	char path_hash[HASH_HEX], env_hash[HASH_HEX], identity_hash[HASH_HEX], input_hash[HASH_HEX];
	sha256_hex_str(proof_path_env(), path_hash);
	if (!deterministic_static_env_hash(env_hash) || !process_identity_signal(identity_hash)) {
		return 0;
	}
	char hostname[256];
	if (gethostname(hostname, sizeof(hostname)) != 0) {
		hostname[0] = '\0';
	}
	hostname[sizeof(hostname) - 1] = '\0';
	byte_buf b = {0};
	int ok = bytes_append_str(&b, inv->argv[0]) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_argv_norm(&b, inv) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.file_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, env_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, hostname) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, identity_hash);
	if (ok) {
		sha256_hex_buf(&b, input_hash);
		snprintf(epoch, 256, "hot-static-env:%s", input_hash);
	}
	bytes_free(&b);
	return ok;
}

static int is_printenv_probe(policy_invocation *inv) {
	return inv->argc == 2 && strcmp(inv->argv[0], "printenv") == 0 && safe_printenv_name(inv->argv[1]);
}

static int printenv_epoch(policy_invocation *inv, char epoch[256]) {
	if (!is_printenv_probe(inv)) {
		return 0;
	}
	executable_signal sig;
	if (!executable_signal_for(inv->cwd, "printenv", &sig)) {
		return 0;
	}
	const char *name = inv->argv[1];
	const char *value = getenv(name);
	const char *exists = value == NULL ? "false" : "true";
	if (value == NULL) {
		value = "";
	}
	char path_hash[HASH_HEX], input_hash[HASH_HEX];
	sha256_hex_str(proof_path_env(), path_hash);
	byte_buf b = {0};
	int ok = bytes_append_str(&b, name) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, exists) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, value) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.file_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, path_hash);
	if (ok) {
		sha256_hex_buf(&b, input_hash);
		snprintf(epoch, 256, "hot-printenv:%s", input_hash);
	}
	bytes_free(&b);
	return ok;
}

static int supported_ls_flag(const char *flag) {
	return strcmp(flag, "-p") == 0;
}

static int clean_ls_relative_path(const char *input, char out[PATH_BUF]) {
	if (input == NULL || input[0] == '\0' || input[0] == '/') {
		return 0;
	}
	char tmp[PATH_BUF];
	snprintf(tmp, sizeof(tmp), "%s", input);
	char *parts[256];
	int count = 0;
	char *save = NULL;
	for (char *part = strtok_r(tmp, "/", &save); part != NULL; part = strtok_r(NULL, "/", &save)) {
		if (part[0] == '\0' || strcmp(part, ".") == 0) {
			continue;
		}
		if (strcmp(part, "..") == 0) {
			return 0;
		}
		if (count >= 256) {
			return 0;
		}
		parts[count++] = part;
	}
	if (count == 0) {
		snprintf(out, PATH_BUF, ".");
		return 1;
	}
	out[0] = '\0';
	for (int i = 0; i < count; i++) {
		size_t used = strlen(out);
		int n = snprintf(out + used, PATH_BUF - used, "%s%s", i > 0 ? "/" : "", parts[i]);
		if (n < 0 || (size_t)n >= PATH_BUF - used) {
			return 0;
		}
	}
	return 1;
}

static int parse_directory_listing(policy_invocation *inv, char target[PATH_BUF], char flag[16]) {
	if (inv->argc < 1 || inv->argc > 3 || strcmp(inv->argv[0], "ls") != 0) {
		return 0;
	}
	const char *path = ".";
	flag[0] = '\0';
	if (inv->argc == 2) {
		if (inv->argv[1][0] == '-') {
			if (!supported_ls_flag(inv->argv[1])) {
				return 0;
			}
			snprintf(flag, 16, "%s", inv->argv[1]);
		} else {
			path = inv->argv[1];
		}
	} else if (inv->argc == 3) {
		if (!supported_ls_flag(inv->argv[1]) || inv->argv[2][0] == '-') {
			return 0;
		}
		snprintf(flag, 16, "%s", inv->argv[1]);
		path = inv->argv[2];
	}
	return clean_ls_relative_path(path, target);
}

static int directory_listing_env_hash(char out[HASH_HEX]) {
	static const char *keys[] = {
		"LC_ALL", "LC_COLLATE", "LC_CTYPE", "LANG", "TZ", "COLUMNS", "CLICOLOR", "CLICOLOR_FORCE",
		"LSCOLORS", "LS_COLORS", "BLOCKSIZE",
	};
	return hash_selected_environment(keys, sizeof(keys) / sizeof(keys[0]), out);
}

static int path_within_root_c(const char *path, const char *root) {
	size_t root_len = strlen(root);
	return strcmp(path, root) == 0 || (strncmp(path, root, root_len) == 0 && path[root_len] == '/');
}

static int directory_entry_epoch(const char *dir, char out[HASH_HEX]) {
	struct stat st;
	if (lstat(dir, &st) != 0 || !S_ISDIR(st.st_mode)) {
		return 0;
	}
	char mode[32], stat_signal[256];
	if (!mode_string(st.st_mode, mode) || !file_stat_signal(&st, mode, stat_signal, sizeof(stat_signal))) {
		return 0;
	}
	string_list parts = {0};
	byte_buf self = {0};
	int ok = bytes_append_str(&self, "self") && bytes_append_byte(&self, 0) && bytes_append_str(&self, stat_signal);
	if (ok) {
		ok = list_add_bytes(&parts, self.data, self.len);
	}
	bytes_free(&self);
	DIR *d = opendir(dir);
	if (!ok || d == NULL) {
		list_free(&parts);
		return 0;
	}
	struct dirent *de;
	while ((de = readdir(d)) != NULL) {
		if (strcmp(de->d_name, ".") == 0 || strcmp(de->d_name, "..") == 0) {
			continue;
		}
		if (parts.len > 2000) {
			closedir(d);
			list_free(&parts);
			return 0;
		}
		char path[PATH_BUF];
		if (!join_path(path, sizeof(path), dir, de->d_name)) {
			closedir(d);
			list_free(&parts);
			return 0;
		}
		struct stat entry_st;
		if (lstat(path, &entry_st) != 0 || !mode_string(entry_st.st_mode, mode) ||
		    !file_stat_signal(&entry_st, mode, stat_signal, sizeof(stat_signal))) {
			closedir(d);
			list_free(&parts);
			return 0;
		}
		char link_target[PATH_BUF] = "";
		if (S_ISLNK(entry_st.st_mode)) {
			ssize_t n = readlink(path, link_target, sizeof(link_target) - 1);
			if (n >= 0) {
				link_target[n] = '\0';
			} else {
				link_target[0] = '\0';
			}
		}
		char size_buf[64];
		snprintf(size_buf, sizeof(size_buf), "%lld", (long long)entry_st.st_size);
		byte_buf line = {0};
		ok = bytes_append_str(&line, de->d_name) &&
		     bytes_append_byte(&line, 0) &&
		     bytes_append_str(&line, S_ISDIR(entry_st.st_mode) ? "true" : "false") &&
		     bytes_append_byte(&line, 0) &&
		     bytes_append_str(&line, size_buf) &&
		     bytes_append_byte(&line, 0) &&
		     bytes_append_str(&line, mode) &&
		     bytes_append_byte(&line, 0) &&
		     bytes_append_str(&line, stat_signal) &&
		     bytes_append_byte(&line, 0) &&
		     bytes_append_str(&line, link_target);
		if (ok) {
			ok = list_add_bytes(&parts, line.data, line.len);
		}
		bytes_free(&line);
		if (!ok) {
			closedir(d);
			list_free(&parts);
			return 0;
		}
	}
	closedir(d);
	ok = hash_joined_lines(&parts, out);
	list_free(&parts);
	return ok;
}

static int directory_listing_epoch(policy_invocation *inv, char epoch[256]) {
	char target[PATH_BUF], flag[16];
	if (!parse_directory_listing(inv, target, flag)) {
		return 0;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF];
	if (!discover_git_dir(inv->cwd, repo_root, git_dir)) {
		return 0;
	}
	char root_real[PATH_BUF];
	if (realpath(repo_root, root_real) == NULL) {
		snprintf(root_real, sizeof(root_real), "%s", repo_root);
	}
	char target_joined[PATH_BUF], target_real[PATH_BUF];
	if (!join_path(target_joined, sizeof(target_joined), inv->cwd, target) || realpath(target_joined, target_real) == NULL) {
		return 0;
	}
	if (!path_within_root_c(target_real, root_real)) {
		return 0;
	}
	struct stat st;
	if (stat(target_real, &st) != 0 || !S_ISDIR(st.st_mode)) {
		return 0;
	}
	executable_signal sig;
	if (!executable_signal_for(inv->cwd, "ls", &sig)) {
		return 0;
	}
	char dir_epoch[HASH_HEX], env_hash[HASH_HEX], passwd_fp[HASH_HEX + 16], group_fp[HASH_HEX + 16], localtime_fp[HASH_HEX + 16], input_hash[HASH_HEX];
	if (!directory_entry_epoch(target_real, dir_epoch) || !directory_listing_env_hash(env_hash)) {
		return 0;
	}
	file_hash_or_missing("/etc/passwd", passwd_fp);
	file_hash_or_missing("/etc/group", group_fp);
	file_hash_or_missing("/etc/localtime", localtime_fp);
	byte_buf b = {0};
	int ok = bytes_append_str(&b, target_real) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_argv_norm(&b, inv) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, dir_epoch) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, env_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.file_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, passwd_fp) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, group_fp) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, localtime_fp);
	if (ok) {
		sha256_hex_buf(&b, input_hash);
		snprintf(epoch, 256, "hot-directory-listing:%s", input_hash);
	}
	bytes_free(&b);
	return ok;
}

static int is_sensitive_name(const char *name) {
	char lower[PATH_BUF];
	size_t n = strlen(name);
	if (n >= sizeof(lower)) {
		return 1;
	}
	for (size_t i = 0; i <= n; i++) {
		lower[i] = (char)tolower((unsigned char)name[i]);
	}
	return strcmp(lower, ".env") == 0 ||
	       strstr(lower, ".env") != NULL ||
	       strstr(lower, ".pem") != NULL ||
	       strstr(lower, ".p12") != NULL ||
	       strstr(lower, ".pfx") != NULL ||
	       strstr(lower, ".key") != NULL ||
	       strstr(lower, "secret") != NULL ||
	       strstr(lower, "credential") != NULL ||
	       strstr(lower, "token") != NULL ||
	       strstr(lower, "api_key") != NULL ||
	       strstr(lower, "apikey") != NULL ||
	       strstr(lower, "private_key") != NULL ||
	       strstr(lower, "privatekey") != NULL ||
	       strcmp(lower, "id_rsa") == 0 ||
	       strcmp(lower, "id_ed25519") == 0;
}

static int has_ext(const char *name, const char *ext) {
	size_t nl = strlen(name), el = strlen(ext);
	return nl >= el && strcasecmp(name + nl - el, ext) == 0;
}

static int is_replayable_name(const char *name) {
	if (is_sensitive_name(name)) {
		return 0;
	}
	static const char *manifest[] = {
		"go.mod", "go.sum", "go.work", "go.work.sum", "package.json", "package-lock.json",
		"npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "tsconfig.json", "Cargo.toml",
		"Cargo.lock", "rust-toolchain", "rust-toolchain.toml", "pyproject.toml", "poetry.lock",
		"requirements.txt", "requirements-dev.txt", "setup.cfg", "tox.ini", "Makefile", "makefile",
		"Dockerfile", "docker-compose.yml", "compose.yml",
	};
	for (size_t i = 0; i < sizeof(manifest) / sizeof(manifest[0]); i++) {
		if (strcmp(name, manifest[i]) == 0) {
			return 1;
		}
	}
	static const char *exts[] = {
		".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".rs", ".java", ".kt",
		".kts", ".rb", ".php", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".swift", ".sh",
		".bash", ".zsh", ".fish", ".sql", ".css", ".scss", ".sass", ".html", ".htm", ".json",
		".jsonc", ".toml", ".yaml", ".yml", ".xml", ".md", ".markdown", ".txt",
	};
	for (size_t i = 0; i < sizeof(exts) / sizeof(exts[0]); i++) {
		if (has_ext(name, exts[i])) {
			return 1;
		}
	}
	return 0;
}

static int parse_sed_range(const char *expr, int *start, int *end) {
	size_t n = strlen(expr);
	if (n < 2 || expr[n - 1] != 'p') {
		return 0;
	}
	char body[64];
	if (n >= sizeof(body)) {
		return 0;
	}
	memcpy(body, expr, n - 1);
	body[n - 1] = '\0';
	char *comma = strchr(body, ',');
	if (comma != NULL) {
		*comma = '\0';
		*start = atoi(body);
		*end = atoi(comma + 1);
	} else {
		*start = atoi(body);
		*end = *start;
	}
	return *start > 0 && *end >= *start && *end - *start <= 500 && *end <= 10000;
}

static int parse_head_tail_count(const char *s, int tail, int *count) {
	if (s == NULL || s[0] == '\0') {
		return 0;
	}
	if (tail && s[0] == '+') {
		return 0;
	}
	int n = 0;
	for (const char *p = s; *p != '\0'; p++) {
		if (!isdigit((unsigned char)*p)) {
			return 0;
		}
		n = n * 10 + (*p - '0');
		if (n > 1000) {
			return 0;
		}
	}
	*count = n;
	return n > 0;
}

static int parse_head_tail_args(policy_invocation *inv, int tail, const char **path, int *count) {
	if (inv->argc < 2 || inv->argc > 4) {
		return 0;
	}
	*count = 10;
	int path_index = 1;
	if (inv->argc >= 3) {
		const char *arg = inv->argv[1];
		if (strcmp(arg, "-n") == 0) {
			if (inv->argc != 4 || !parse_head_tail_count(inv->argv[2], tail, count)) {
				return 0;
			}
			path_index = 3;
		} else if (strncmp(arg, "-n", 2) == 0 && strlen(arg) > 2) {
			if (inv->argc != 3 || !parse_head_tail_count(arg + 2, tail, count)) {
				return 0;
			}
			path_index = 2;
		} else if (arg[0] == '-' && arg[1] != '\0') {
			if (inv->argc != 3 || !parse_head_tail_count(arg + 1, tail, count)) {
				return 0;
			}
			path_index = 2;
		} else {
			return 0;
		}
	}
	if (path_index >= inv->argc) {
		return 0;
	}
	*path = inv->argv[path_index];
	return 1;
}

static int is_file_type_candidate(policy_invocation *inv) {
	if (inv->argc != 2 || strcmp(inv->argv[0], "file") != 0) {
		return 0;
	}
	char rel[PATH_BUF];
	if (!clean_relative_path(inv->argv[1], rel)) {
		return 0;
	}
	return is_replayable_name(base_name(rel));
}

static int file_type_epoch(policy_invocation *inv, char epoch[256]) {
	if (!is_file_type_candidate(inv)) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: file skip candidate argc=%d argv0=%s\n",
			        inv != NULL ? inv->argc : -1,
			        (inv != NULL && inv->argc > 0) ? inv->argv[0] : "");
		}
		return 0;
	}
	char rel[PATH_BUF];
	if (!clean_relative_path(inv->argv[1], rel)) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: file skip rel path=%s\n", inv->argv[1]);
		}
		return 0;
	}
	char root_real[PATH_BUF], path_joined[PATH_BUF], path_real[PATH_BUF];
	if (realpath(inv->cwd, root_real) == NULL ||
	    !join_path(path_joined, sizeof(path_joined), inv->cwd, rel) ||
	    realpath(path_joined, path_real) == NULL ||
	    !path_within_root_c(path_real, root_real)) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: file skip path cwd=%s rel=%s\n", inv->cwd, rel);
		}
		return 0;
	}
	struct stat st;
	if (stat(path_real, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size < 0 || st.st_size > MAX_WARM_FILE_BYTES) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: file skip stat path=%s\n", path_real);
		}
		return 0;
	}
	char content_hash[HASH_HEX], mode[32], env_hash[HASH_HEX], input_hash[HASH_HEX];
	if (!read_file_hash(path_real, content_hash, NULL, NULL) || !mode_string(st.st_mode, mode) || !file_command_env_hash(env_hash)) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: file skip local-proof path=%s\n", path_real);
		}
		return 0;
	}
	executable_signal sig;
	if (!executable_signal_for(inv->cwd, "file", &sig)) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: file skip tool cwd=%s\n", inv->cwd);
		}
		return 0;
	}
	char size_buf[64];
	snprintf(size_buf, sizeof(size_buf), "%lld", (long long)st.st_size);
	byte_buf b = {0};
	int ok = bytes_append_str(&b, inv->cwd) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, rel) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, content_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, size_buf) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, mode) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_argv_norm(&b, inv) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.path_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, sig.file_hash) &&
	         bytes_append_byte(&b, '|') &&
	         bytes_append_str(&b, env_hash);
	if (ok) {
		sha256_hex_buf(&b, input_hash);
		snprintf(epoch, 256, "hot-file-inspection:%s", input_hash);
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: file path=%s rel=%s content=%s size=%s mode=%s tool_path=%s tool=%s env=%s epoch=%s\n",
			        path_real, rel, content_hash, size_buf, mode, sig.path_hash, sig.file_hash, env_hash, epoch);
		}
	}
	bytes_free(&b);
	return ok;
}

static int parse_fixed_grep_args(policy_invocation *inv, const char **pattern, const char **path, int *quiet) {
	if (inv->argc != 4 && inv->argc != 5) {
		return 0;
	}
	if (strcmp(inv->argv[0], "grep") != 0) {
		return 0;
	}
	*quiet = 0;
	if (inv->argc == 4 && strcmp(inv->argv[1], "-F") == 0) {
		*pattern = inv->argv[2];
		*path = inv->argv[3];
	} else if (inv->argc == 5 && strcmp(inv->argv[1], "-F") == 0 && strcmp(inv->argv[2], "-q") == 0) {
		*quiet = 1;
		*pattern = inv->argv[3];
		*path = inv->argv[4];
	} else if (inv->argc == 5 && strcmp(inv->argv[1], "-q") == 0 && strcmp(inv->argv[2], "-F") == 0) {
		*quiet = 1;
		*pattern = inv->argv[3];
		*path = inv->argv[4];
	} else {
		return 0;
	}
	if (*pattern == NULL || (*pattern)[0] == '\0' || (*pattern)[0] == '-' || strchr(*pattern, '\n') != NULL || strchr(*pattern, '\r') != NULL) {
		return 0;
	}
	char rel[PATH_BUF];
	if (!clean_relative_path(*path, rel) || !is_replayable_name(base_name(rel))) {
		return 0;
	}
	return 1;
}

static int parse_fixed_rg_args(policy_invocation *inv, const char **pattern, const char **path, int *quiet, int *line_number) {
	if (inv->argc < 4 || inv->argc > 6 || strcmp(inv->argv[0], "rg") != 0) {
		return 0;
	}
	*pattern = NULL;
	*path = NULL;
	*quiet = 0;
	*line_number = 0;
	int fixed = 0;
	for (int i = 1; i < inv->argc; i++) {
		if (strcmp(inv->argv[i], "-F") == 0 || strcmp(inv->argv[i], "--fixed-strings") == 0) {
			if (fixed) {
				return 0;
			}
			fixed = 1;
			continue;
		}
		if (strcmp(inv->argv[i], "-q") == 0 || strcmp(inv->argv[i], "--quiet") == 0) {
			if (*quiet) {
				return 0;
			}
			*quiet = 1;
			continue;
		}
		if (strcmp(inv->argv[i], "-n") == 0 || strcmp(inv->argv[i], "--line-number") == 0) {
			if (*line_number) {
				return 0;
			}
			*line_number = 1;
			continue;
		}
		if (inv->argv[i][0] == '-') {
			return 0;
		}
		if (*pattern == NULL) {
			*pattern = inv->argv[i];
			continue;
		}
		if (*path != NULL) {
			return 0;
		}
		*path = inv->argv[i];
	}
	if (!fixed || *pattern == NULL || *path == NULL || (*pattern)[0] == '\0' ||
	    strchr(*pattern, '\n') != NULL || strchr(*pattern, '\r') != NULL) {
		return 0;
	}
	char rel[PATH_BUF];
	if (!clean_relative_path(*path, rel) || !is_replayable_name(base_name(rel))) {
		return 0;
	}
	return 1;
}

static int warm_file_replay_enabled(void) {
	return env_truthy("SQUIRE_SHIM_ENABLE_WARM_FILE_REPLAY") || env_truthy("SQUIRE_SHIM_REQUIRE_HIT");
}

static int is_warm_file_candidate(policy_invocation *inv) {
	if (inv->argc == 2 && strcmp(inv->argv[0], "cat") == 0) {
		return 1;
	}
	if (inv->argc == 4 && strcmp(inv->argv[0], "sed") == 0 && strcmp(inv->argv[1], "-n") == 0) {
		int start, end;
		return parse_sed_range(inv->argv[2], &start, &end);
	}
	if (strcmp(inv->argv[0], "head") == 0 || strcmp(inv->argv[0], "tail") == 0) {
		const char *path = NULL;
		int count = 0;
		return parse_head_tail_args(inv, strcmp(inv->argv[0], "tail") == 0, &path, &count);
	}
	if (strcmp(inv->argv[0], "grep") == 0) {
		const char *pattern = NULL;
		const char *path = NULL;
		int quiet = 0;
		return parse_fixed_grep_args(inv, &pattern, &path, &quiet);
	}
	if (strcmp(inv->argv[0], "rg") == 0) {
		const char *pattern = NULL;
		const char *path = NULL;
		int quiet = 0;
		int line_number = 0;
		return parse_fixed_rg_args(inv, &pattern, &path, &quiet, &line_number);
	}
	return 0;
}

static int warm_file_proof(policy_invocation *inv, char key[HASH_HEX], char epoch[256], char path[PATH_BUF], int *sed_start, int *sed_end, int *line_count) {
	const char *arg_path = NULL;
	*sed_start = 0;
	*sed_end = 0;
	*line_count = 0;
	if (inv->argc == 2 && strcmp(inv->argv[0], "cat") == 0) {
		arg_path = inv->argv[1];
	} else if (inv->argc == 4 && strcmp(inv->argv[0], "sed") == 0 && strcmp(inv->argv[1], "-n") == 0) {
		if (!parse_sed_range(inv->argv[2], sed_start, sed_end)) {
			return 0;
		}
		arg_path = inv->argv[3];
	} else if (strcmp(inv->argv[0], "head") == 0 || strcmp(inv->argv[0], "tail") == 0) {
		if (!parse_head_tail_args(inv, strcmp(inv->argv[0], "tail") == 0, &arg_path, line_count)) {
			return 0;
		}
	} else if (strcmp(inv->argv[0], "grep") == 0) {
		const char *pattern = NULL;
		int quiet = 0;
		if (!parse_fixed_grep_args(inv, &pattern, &arg_path, &quiet)) {
			return 0;
		}
	} else if (strcmp(inv->argv[0], "rg") == 0) {
		const char *pattern = NULL;
		int quiet = 0;
		int line_number = 0;
		if (!parse_fixed_rg_args(inv, &pattern, &arg_path, &quiet, &line_number)) {
			return 0;
		}
	} else {
		return 0;
	}
	char rel[PATH_BUF];
	if (!clean_relative_path(arg_path, rel)) {
		return 0;
	}
	const char *name = base_name(rel);
	if (!is_replayable_name(name)) {
		return 0;
	}
	if (!join_path(path, PATH_BUF, inv->cwd, rel)) {
		return 0;
	}
	char root_real[PATH_BUF], path_real[PATH_BUF];
	if (realpath(inv->cwd, root_real) == NULL || realpath(path, path_real) == NULL || !path_within_root_c(path_real, root_real)) {
		return 0;
	}
	snprintf(path, PATH_BUF, "%s", path_real);
	struct stat st;
	if (stat(path, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size < 0 || st.st_size > MAX_WARM_FILE_BYTES) {
		return 0;
	}
	if (strcmp(inv->argv[0], "cat") == 0 && st.st_size > MAX_FAST_OUTPUT_BYTES) {
		return 0;
	}
	char content_hash[HASH_HEX];
	if (!read_file_hash(path, content_hash, NULL, NULL)) {
		return 0;
	}
	char mode[16];
	if (!mode_string(st.st_mode, mode)) {
		return 0;
	}
	char key_input[PATH_BUF * 2];
	snprintf(key_input, sizeof(key_input), "%s%c%s", inv->cwd, 0, rel);
	sha256_hex_bytes((const unsigned char *)key_input, strlen(inv->cwd) + 1 + strlen(rel), key);
	char epoch_input[PATH_BUF * 3];
	snprintf(epoch_input, sizeof(epoch_input), "%s|%s|%s|%lld|%s", inv->cwd, rel, content_hash, (long long)st.st_size, mode);
	char epoch_hash[HASH_HEX];
	sha256_hex_str(epoch_input, epoch_hash);
	snprintf(epoch, 256, "hot-warm-file:%s", epoch_hash);
	return 1;
}

static int map_snapshot_fd(int fd, mapped_snapshot *snap) {
	struct stat st;
	if (fstat(fd, &st) != 0 || st.st_size < HOT_HEADER_BYTES || st.st_size > HOT_MAX_BYTES) {
		return 0;
	}
	unsigned char *data = mmap(NULL, (size_t)st.st_size, PROT_READ, MAP_SHARED, fd, 0);
	if (data == MAP_FAILED) {
		return 0;
	}
	snap->data = data;
	snap->len = (size_t)st.st_size;
	snap->borrowed = 0;
	return 1;
}

static int map_snapshot_fd_cached(int fd, mapped_snapshot *snap) {
	static int cached_fd = -1;
	static unsigned char *cached_data;
	static size_t cached_len;
	if (cached_fd == fd && cached_data != NULL && cached_len >= HOT_HEADER_BYTES) {
		snap->data = cached_data;
		snap->len = cached_len;
		snap->borrowed = 1;
		return 1;
	}
	struct stat st;
	if (fstat(fd, &st) != 0 || st.st_size < HOT_HEADER_BYTES || st.st_size > HOT_MAX_BYTES) {
		return 0;
	}
	unsigned char *data = mmap(NULL, (size_t)st.st_size, PROT_READ, MAP_SHARED, fd, 0);
	if (data == MAP_FAILED) {
		return 0;
	}
	cached_fd = fd;
	cached_data = data;
	cached_len = (size_t)st.st_size;
	snap->data = cached_data;
	snap->len = cached_len;
	snap->borrowed = 1;
	return 1;
}

static int map_snapshot(const char *store_root, mapped_snapshot *snap) {
	int inherited_fd = hot_snapshot_fd();
	if (inherited_fd >= 0 && map_snapshot_fd_cached(inherited_fd, snap)) {
		mmap_trace_path("snapshot-fd-ok", NULL);
		return 1;
	}
	char snapshot_path[PATH_BUF];
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
	snap->data = data;
	snap->len = (size_t)st.st_size;
	snap->borrowed = 0;
	return 1;
}

static void unmap_snapshot(mapped_snapshot *snap) {
	if (snap->data != NULL && !snap->borrowed) {
		munmap(snap->data, snap->len);
	}
	snap->data = NULL;
	snap->len = 0;
	snap->borrowed = 0;
}

static int snapshot_header(mapped_snapshot *snap, uint32_t *count, uint32_t *payload_offset, uint32_t *total_size) {
	unsigned char *data = snap->data;
	if (snap->len < HOT_HEADER_BYTES || le64(data) != HOT_MAGIC || le16(data + 8) != HOT_VERSION || le16(data + 10) != HOT_ENTRY_BYTES) {
		return 0;
	}
	*count = le32(data + 12);
	uint32_t header_size = le32(data + 16);
	*payload_offset = le32(data + 20);
	*total_size = le32(data + 24);
	if (*count > HOT_MAX_ENTRIES || header_size != HOT_HEADER_BYTES || *total_size != (uint32_t)snap->len ||
	    *payload_offset != HOT_HEADER_BYTES + *count * HOT_ENTRY_BYTES || *payload_offset > *total_size) {
		return 0;
	}
	return 1;
}

static int snapshot_key_compare(const unsigned char *entry_key, const char command_hash[HASH_HEX]) {
	return memcmp(entry_key, command_hash, 64);
}

static int snapshot_command_start(mapped_snapshot *snap, uint32_t count, const char command_hash[HASH_HEX], uint32_t *start) {
	uint32_t lo = 0;
	uint32_t hi = count;
	while (lo < hi) {
		uint32_t mid = lo + (hi - lo) / 2;
		unsigned char *entry = snap->data + HOT_HEADER_BYTES + mid * HOT_ENTRY_BYTES;
		if (snapshot_key_compare(entry, command_hash) < 0) {
			lo = mid + 1;
		} else {
			hi = mid;
		}
	}
	if (lo >= count) {
		return 0;
	}
	unsigned char *entry = snap->data + HOT_HEADER_BYTES + lo * HOT_ENTRY_BYTES;
	if (snapshot_key_compare(entry, command_hash) != 0) {
		return 0;
	}
	*start = lo;
	return 1;
}

static int snapshot_find(mapped_snapshot *snap, const char command_hash[HASH_HEX], const char epoch[256], uint32_t want_kind,
                         const unsigned char **stdout_data, uint32_t *stdout_len, const unsigned char **stderr_data,
                         uint32_t *stderr_len, int *exit_code, uint64_t *native_wall_ms) {
	uint32_t count, payload_offset, total_size;
	if (!snapshot_header(snap, &count, &payload_offset, &total_size)) {
		return 0;
	}
	char epoch_hash[HASH_HEX];
	sha256_hex_str(epoch, epoch_hash);
	uint32_t start = 0;
	if (!snapshot_command_start(snap, count, command_hash, &start)) {
		return 0;
	}
	for (uint32_t i = start; i < count; i++) {
		unsigned char *entry = snap->data + HOT_HEADER_BYTES + i * HOT_ENTRY_BYTES;
		int key_cmp = snapshot_key_compare(entry, command_hash);
		if (key_cmp != 0) {
			break;
		}
		if (memcmp(entry + 64, epoch_hash, 64) != 0) {
			continue;
		}
		char stdout_hash[HASH_HEX];
		char stderr_hash[HASH_HEX];
		memcpy(stdout_hash, entry + 128, 64);
		memcpy(stderr_hash, entry + 192, 64);
		stdout_hash[64] = '\0';
		stderr_hash[64] = '\0';
		if (!valid_hex64(stdout_hash) || !valid_hex64(stderr_hash)) {
			return 0;
		}
		uint32_t so = le32(entry + 256);
		uint32_t sl = le32(entry + 260);
		uint32_t eo = le32(entry + 264);
		uint32_t el = le32(entry + 268);
		uint32_t kind = le32(entry + 276);
		if (kind != want_kind || so < payload_offset || eo < payload_offset || so > total_size || eo > total_size ||
		    sl > total_size - so || el > total_size - eo) {
			return 0;
		}
		char got_stdout[HASH_HEX];
		char got_stderr[HASH_HEX];
		sha256_hex_bytes(snap->data + so, sl, got_stdout);
		sha256_hex_bytes(snap->data + eo, el, got_stderr);
		if (strcmp(got_stdout, stdout_hash) != 0 || strcmp(got_stderr, stderr_hash) != 0) {
			return 0;
		}
		*stdout_data = snap->data + so;
		*stdout_len = sl;
		*stderr_data = snap->data + eo;
		*stderr_len = el;
		*exit_code = (int)(int32_t)le32(entry + 272);
		if (native_wall_ms != NULL) {
			*native_wall_ms = le64(entry + 280);
		}
		return 1;
	}
	return 0;
}

static int output_sed_range(const unsigned char *content, uint32_t len, int start, int end) {
	int line = 1;
	uint32_t offset = 0;
	while (offset < len && line <= end) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		if (line_end < len && content[line_end] == '\n') {
			line_end++;
		}
		if (line >= start) {
			if (!write_all(STDOUT_FILENO, content + offset, line_end - offset)) {
				return 0;
			}
		}
		offset = line_end;
		line++;
	}
	return 1;
}

static int count_lines(const unsigned char *content, uint32_t len) {
	int lines = 0;
	uint32_t offset = 0;
	while (offset < len) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		if (line_end < len && content[line_end] == '\n') {
			line_end++;
		}
		offset = line_end;
		lines++;
	}
	return lines;
}

static int output_tail_lines(const unsigned char *content, uint32_t len, int count) {
	if (count < 1 || len == 0) {
		return 1;
	}
	int total = count_lines(content, len);
	int start = total - count + 1;
	if (start < 1) {
		start = 1;
	}
	return output_sed_range(content, len, start, total);
}

static int mem_contains_bytes(const unsigned char *haystack, uint32_t haystack_len, const unsigned char *needle, size_t needle_len) {
	if (needle_len == 0 || haystack_len < needle_len) {
		return 0;
	}
	for (uint32_t i = 0; i + needle_len <= haystack_len; i++) {
		if (memcmp(haystack + i, needle, needle_len) == 0) {
			return 1;
		}
	}
	return 0;
}

static int output_fixed_grep(const unsigned char *content, uint32_t len, const char *pattern, int quiet, int *matched) {
	*matched = 0;
	size_t pattern_len = strlen(pattern);
	if (pattern_len == 0) {
		return 0;
	}
	for (uint32_t i = 0; i < len; i++) {
		if (content[i] == '\0') {
			return 0;
		}
	}
	uint32_t offset = 0;
	while (offset < len) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		uint32_t line_match_len = line_end - offset;
		uint32_t line_write_len = line_match_len;
		if (mem_contains_bytes(content + offset, line_match_len, (const unsigned char *)pattern, pattern_len)) {
			*matched = 1;
			if (quiet) {
				return 1;
			}
			if (line_write_len > 0 && !write_all(STDOUT_FILENO, content + offset, line_write_len)) {
				return 0;
			}
			if (!write_all(STDOUT_FILENO, "\n", 1)) {
				return 0;
			}
		}
		if (line_end < len && content[line_end] == '\n') {
			line_end++;
		}
		offset = line_end;
	}
	return 1;
}

static int write_decimal_int(int value) {
	char buf[32];
	int n = snprintf(buf, sizeof(buf), "%d", value);
	if (n <= 0 || n >= (int)sizeof(buf)) {
		return 0;
	}
	return write_all(STDOUT_FILENO, buf, (size_t)n);
}

static int output_fixed_rg(const unsigned char *content, uint32_t len, const char *pattern, int quiet, int line_number, int *matched) {
	*matched = 0;
	size_t pattern_len = strlen(pattern);
	if (pattern_len == 0) {
		return 0;
	}
	for (uint32_t i = 0; i < len; i++) {
		if (content[i] == '\0') {
			return 0;
		}
	}
	int line = 1;
	uint32_t offset = 0;
	while (offset < len) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		uint32_t line_match_len = line_end - offset;
		if (mem_contains_bytes(content + offset, line_match_len, (const unsigned char *)pattern, pattern_len)) {
			*matched = 1;
			if (quiet) {
				return 1;
			}
			if (line_number && (!write_decimal_int(line) || !write_all(STDOUT_FILENO, ":", 1))) {
				return 0;
			}
			if (line_match_len > 0 && !write_all(STDOUT_FILENO, content + offset, line_match_len)) {
				return 0;
			}
			if (!write_all(STDOUT_FILENO, "\n", 1)) {
				return 0;
			}
		}
		if (line_end < len && content[line_end] == '\n') {
			line_end++;
		}
		offset = line_end;
		line++;
	}
	return 1;
}

static int discover_store_root(const char *cwd, char store_root[PATH_BUF]) {
	const char *env = getenv("SQUIRE_KERNEL_STORE_ROOT");
	if (env != NULL && env[0] != '\0') {
		snprintf(store_root, PATH_BUF, "%s", env);
		return 1;
	}
	env = getenv("SQUIRE_STORE_ROOT");
	if (env != NULL && env[0] != '\0') {
		snprintf(store_root, PATH_BUF, "%s", env);
		return 1;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF];
	if (!discover_git_dir(cwd, repo_root, git_dir)) {
		return 0;
	}
	return join_path(store_root, PATH_BUF, git_dir, "squire/kernel");
}

static int replay_exact(mapped_snapshot *snap, policy_invocation *inv, const char epoch[256], const char *store_root, long long replay_start_ns) {
	char key[HASH_HEX];
	command_key(inv, key);
	if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
		char epoch_hash[HASH_HEX];
		sha256_hex_str(epoch, epoch_hash);
		fprintf(stderr, "squire mmap proof debug: key=%s epoch=%s epoch_hash=%s\n", key, epoch, epoch_hash);
	}
	const unsigned char *out, *err;
	uint32_t out_len, err_len;
	int exit_code;
	uint64_t native_wall_ms = 0;
	if (!snapshot_find(snap, key, epoch, HOT_KIND_EXACT, &out, &out_len, &err, &err_len, &exit_code, &native_wall_ms)) {
		return 0;
	}
	if (out_len + err_len > MAX_FAST_OUTPUT_BYTES) {
		return 0;
	}
	if (out_len > 0 && !write_all(STDOUT_FILENO, out, out_len)) {
		return 0;
	}
	if (err_len > 0 && !write_all(STDERR_FILENO, err, err_len)) {
		return 0;
	}
	record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
	_exit(exit_code);
}

static int replay_warm_file(mapped_snapshot *snap, policy_invocation *inv, const char *store_root, long long replay_start_ns) {
	char key[HASH_HEX], epoch[256], path[PATH_BUF];
	int sed_start, sed_end, line_count;
	if (!warm_file_proof(inv, key, epoch, path, &sed_start, &sed_end, &line_count)) {
		return 0;
	}
	char command_hash[HASH_HEX];
	char command_input[128];
	snprintf(command_input, sizeof(command_input), "warm-file:%s", key);
	sha256_hex_str(command_input, command_hash);
	const unsigned char *content, *err;
	uint32_t content_len, err_len;
	int exit_code;
	uint64_t native_wall_ms = 0;
	if (!snapshot_find(snap, command_hash, epoch, HOT_KIND_WARM_FILE, &content, &content_len, &err, &err_len, &exit_code, &native_wall_ms)) {
		return 0;
	}
	if (err_len != 0 || exit_code != 0) {
		return 0;
	}
	if (strcmp(inv->argv[0], "cat") == 0) {
		if (content_len > MAX_FAST_OUTPUT_BYTES) {
			return 0;
		}
		if (content_len > 0 && !write_all(STDOUT_FILENO, content, content_len)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (strcmp(inv->argv[0], "sed") == 0) {
		if (!output_sed_range(content, content_len, sed_start, sed_end)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (strcmp(inv->argv[0], "head") == 0) {
		if (!output_sed_range(content, content_len, 1, line_count)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (strcmp(inv->argv[0], "tail") == 0) {
		if (!output_tail_lines(content, content_len, line_count)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (strcmp(inv->argv[0], "grep") == 0) {
		const char *pattern = NULL;
		const char *path = NULL;
		int quiet = 0;
		if (!parse_fixed_grep_args(inv, &pattern, &path, &quiet)) {
			return 0;
		}
		int matched = 0;
		if (!output_fixed_grep(content, content_len, pattern, quiet, &matched)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(matched ? 0 : 1);
	}
	if (strcmp(inv->argv[0], "rg") == 0) {
		const char *pattern = NULL;
		const char *path = NULL;
		int quiet = 0;
		int line_number = 0;
		if (!parse_fixed_rg_args(inv, &pattern, &path, &quiet, &line_number)) {
			return 0;
		}
		int matched = 0;
		if (!output_fixed_rg(content, content_len, pattern, quiet, line_number, &matched)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(matched ? 0 : 1);
	}
	return 0;
}

static int prepare_exact_replay_for_epoch(mapped_snapshot *snap, policy_invocation *inv, const char epoch[256], prepared_exact_replay *prepared) {
	char key[HASH_HEX];
	command_key(inv, key);
	const unsigned char *out, *err;
	uint32_t out_len, err_len;
	int exit_code;
	uint64_t native_wall_ms = 0;
	if (!snapshot_find(snap, key, epoch, HOT_KIND_EXACT, &out, &out_len, &err, &err_len, &exit_code, &native_wall_ms)) {
		return 0;
	}
	if (out_len + err_len > MAX_FAST_OUTPUT_BYTES) {
		return 0;
	}
	prepared->stdout_data = out;
	prepared->stderr_data = err;
	prepared->stdout_len = out_len;
	prepared->stderr_len = err_len;
	prepared->exit_code = exit_code;
	prepared->native_wall_ms = native_wall_ms;
	return 1;
}

static int prepare_exact_replay_at_cwd(const char *cwd, int argc, char **argv, prepared_exact_replay *prepared) {
	if (prepared == NULL) {
		return 0;
	}
	memset(prepared, 0, sizeof(*prepared));
	prepared->replay_start_ns = now_monotonic_ns();
	policy_invocation inv;
	if (!normalize_invocation_at_cwd(cwd, argc, argv, &inv)) {
		return 0;
	}
	if (!discover_store_root(inv.cwd, prepared->store_root)) {
		return 0;
	}
	if (!map_snapshot(prepared->store_root, &prepared->snap)) {
		return 0;
	}
	char epoch[256];
	if (git_metadata_epoch(&inv, epoch) && prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
		prepared->synthetic_safe = 1;
		return 1;
	}
	if (repo_summary_epoch(&inv, epoch) && prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
		prepared->synthetic_safe = 1;
		return 1;
	}
	if (tool_version_epoch(&inv, epoch) && prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
		prepared->synthetic_safe = 1;
		return 1;
	}
	if (command_path_epoch(&inv, epoch) && prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
		prepared->synthetic_safe = 1;
		return 1;
	}
	if (static_environment_epoch(&inv, epoch) && prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
		prepared->synthetic_safe = 1;
		return 1;
	}
	if (printenv_epoch(&inv, epoch) && prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
		prepared->synthetic_safe = 1;
		return 1;
	}
	if (directory_listing_epoch(&inv, epoch) && prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
		prepared->synthetic_safe = 1;
		return 1;
	}
	if (file_type_epoch(&inv, epoch) && prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
		prepared->synthetic_safe = 1;
		return 1;
	}
	unmap_snapshot(&prepared->snap);
	memset(prepared, 0, sizeof(*prepared));
	return 0;
}

static int prepare_exact_replay(int argc, char **argv, prepared_exact_replay *prepared) {
	return prepare_exact_replay_at_cwd(NULL, argc, argv, prepared);
}

static void release_prepared_exact_replay(prepared_exact_replay *prepared) {
	if (prepared == NULL) {
		return;
	}
	unmap_snapshot(&prepared->snap);
	memset(prepared, 0, sizeof(*prepared));
}

static void emit_prepared_exact_replay(prepared_exact_replay *prepared) {
	if (prepared == NULL) {
		_exit(127);
	}
	if (prepared->stdout_len > 0 && !write_all(STDOUT_FILENO, prepared->stdout_data, prepared->stdout_len)) {
		_exit(127);
	}
	if (prepared->stderr_len > 0 && !write_all(STDERR_FILENO, prepared->stderr_data, prepared->stderr_len)) {
		_exit(127);
	}
	record_hot_replay_event(prepared->store_root, (long long)prepared->native_wall_ms, prepared->replay_start_ns);
	_exit(prepared->exit_code);
}

static int try_replay(int argc, char **argv) {
	long long replay_start_ns = now_monotonic_ns();
	policy_invocation inv;
	if (!normalize_invocation(argc, argv, &inv)) {
		return 0;
	}
	if (is_warm_file_candidate(&inv) && !warm_file_replay_enabled()) {
		return 0;
	}
	char store_root[PATH_BUF];
	if (!discover_store_root(inv.cwd, store_root)) {
		return 0;
	}
	mapped_snapshot snap = {0};
	if (!map_snapshot(store_root, &snap)) {
		return 0;
	}
	int ok = 0;
	char epoch[256];
	if (git_metadata_epoch(&inv, epoch)) {
		ok = replay_exact(&snap, &inv, epoch, store_root, replay_start_ns);
	}
	if (!ok && repo_summary_epoch(&inv, epoch)) {
		ok = replay_exact(&snap, &inv, epoch, store_root, replay_start_ns);
	}
	if (!ok && tool_version_epoch(&inv, epoch)) {
		ok = replay_exact(&snap, &inv, epoch, store_root, replay_start_ns);
	}
	if (!ok && command_path_epoch(&inv, epoch)) {
		ok = replay_exact(&snap, &inv, epoch, store_root, replay_start_ns);
	}
	if (!ok && static_environment_epoch(&inv, epoch)) {
		ok = replay_exact(&snap, &inv, epoch, store_root, replay_start_ns);
	}
	if (!ok && printenv_epoch(&inv, epoch)) {
		ok = replay_exact(&snap, &inv, epoch, store_root, replay_start_ns);
	}
	if (!ok && directory_listing_epoch(&inv, epoch)) {
		ok = replay_exact(&snap, &inv, epoch, store_root, replay_start_ns);
	}
	if (!ok && file_type_epoch(&inv, epoch)) {
		ok = replay_exact(&snap, &inv, epoch, store_root, replay_start_ns);
	}
	if (!ok) {
		ok = replay_warm_file(&snap, &inv, store_root, replay_start_ns);
	}
	unmap_snapshot(&snap);
	return ok;
}

#if defined(SQUIRE_MMAP_STANDALONE) && !defined(SQUIRE_MMAP_EMBEDDED) && !defined(SQUIRE_MMAP_NO_MAIN)
int main(int argc, char **argv) {
	if (!try_replay(argc, argv)) {
		if (getenv("SQUIRE_SHIM_REQUIRE_HIT") != NULL) {
			fprintf(stderr, "squire mmap proof: hot snapshot miss\n");
			return 91;
		}
		exec_real_command(argc, argv);
	}
	return 0;
}
#endif
