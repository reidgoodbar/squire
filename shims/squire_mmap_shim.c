// Internal mmap proof engine used by Squire's preload transport.
//
// The shim is intentionally local and fault-open. It serves only entries whose
// current invalidation proof can be recomputed in this process. Everything else
// execs the real command with no semantic approximation.
//
// Supported direct-mmap surfaces:
//   - enabled Git metadata fast paths
//   - proof-gated Git repo summaries: ls-files, status, diff
//   - warmed bounded file inspection: cat/head/tail <file>, sed ranges, fused nl -ba | sed ranges
//   - warmed literal grep/rg checks and native-precomputed file(1) type inspection
//   - common tool version probes and command path lookups
//   - static environment probes, printenv <safe-var>, and tight directory listings
//
// Required/optional launcher environment:
//   SQUIRE_STORE_ROOT       optional; otherwise discovered as <gitdir>/squire/state
//   SQUIRE_SHIM_REAL_PATH   optional native PATH used for proof and fallback
//   SQUIRE_REAL_<TOOL>      optional exact native binary path, e.g. SQUIRE_REAL_GIT

#include <ctype.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <fnmatch.h>
#include <limits.h>
#include <pthread.h>
#include <regex.h>
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

extern char **environ;

#if defined(__APPLE__)
#include <CommonCrypto/CommonDigest.h>
#include <sys/event.h>
#elif defined(__linux__)
#include <sys/inotify.h>
#endif

#if defined(__APPLE__)
typedef struct {
	CC_SHA256_CTX inner;
	int failed;
} SQUIRE_SHA256_CTX;

static void SQUIRE_SHA256_Init(SQUIRE_SHA256_CTX *ctx) {
	ctx->failed = CC_SHA256_Init(&ctx->inner) != 1;
}

static void SQUIRE_SHA256_Update(SQUIRE_SHA256_CTX *ctx, const void *data, size_t len) {
	if (!ctx->failed && len > 0 && CC_SHA256_Update(&ctx->inner, data, (CC_LONG)len) != 1) {
		ctx->failed = 1;
	}
}

static void SQUIRE_SHA256_Final(unsigned char digest[32], SQUIRE_SHA256_CTX *ctx) {
	if (ctx->failed || CC_SHA256_Final(digest, &ctx->inner) != 1) {
		memset(digest, 0, 32);
	}
}
#else
#include <openssl/evp.h>
typedef struct {
	EVP_MD_CTX *inner;
	int failed;
} SQUIRE_SHA256_CTX;

static void SQUIRE_SHA256_Init(SQUIRE_SHA256_CTX *ctx) {
	ctx->inner = EVP_MD_CTX_new();
	ctx->failed = ctx->inner == NULL || EVP_DigestInit_ex(ctx->inner, EVP_sha256(), NULL) != 1;
}

static void SQUIRE_SHA256_Update(SQUIRE_SHA256_CTX *ctx, const void *data, size_t len) {
	if (!ctx->failed && len > 0 && EVP_DigestUpdate(ctx->inner, data, len) != 1) {
		ctx->failed = 1;
	}
}

static void SQUIRE_SHA256_Final(unsigned char digest[32], SQUIRE_SHA256_CTX *ctx) {
	unsigned int digest_len = 0;
	if (ctx->failed || EVP_DigestFinal_ex(ctx->inner, digest, &digest_len) != 1 || digest_len != 32) {
		memset(digest, 0, 32);
	}
	EVP_MD_CTX_free(ctx->inner);
	ctx->inner = NULL;
}
#endif

#define HOT_MAGIC UINT64_C(0x3150535148535153)
#define HOT_VERSION 1
#define HOT_HEADER_BYTES 64
#define HOT_ENTRY_BYTES 320
#define HOT_MAX_ENTRIES 8192
#define HOT_MAX_BYTES (64 * 1024 * 1024)
#define HOT_KIND_EXACT 1
#define HOT_KIND_WARM_FILE 2
#define HOT_KIND_REPO_SEARCH_CORPUS 3
#define HOT_KIND_GIT_HISTORY_CORPUS 4
#define HOT_CLIENT_STATS_MAX_BYTES (1024 * 1024)
#define MAX_FAST_OUTPUT_BYTES (1024 * 1024)
#define MAX_FILE_OUTPUT_BYTES (64 * 1024)
#define MAX_WARM_FILE_BYTES (256 * 1024)
#define MAX_REPO_SEARCH_CORPUS_BYTES (48 * 1024 * 1024)
#define MAX_GIT_HISTORY_CORPUS_BYTES (8 * 1024 * 1024)
#define MAX_COMPOSED_INTERMEDIATE_BYTES (2 * 1024 * 1024)
#define MAX_EXECUTABLE_HASH_BYTES (64 * 1024 * 1024)
#define MAX_PREPARE_REQUEST_BYTES (64 * 1024)
#define MAX_ARGC 64
#define PATH_BUF 4096
#define HASH_HEX 65
#define HOT_CLIENT_PROOF_C_MMAP "c-mmap-hot-snapshot"
#define HOT_CLIENT_PROOF_C_SYNTHETIC "c-mmap-hot-synthetic"
#define HOT_CLIENT_PROOF_C_CURRENT_FILE "c-current-file"

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
	void *cache_token;
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
static long long stat_ctime_nano(const struct stat *st);
static int file_stat_signal(const struct stat *st, const char *mode, char *out, size_t cap);
static int join_path(char *out, size_t cap, const char *left, const char *right);
static int safe_relative_inspection_path_arg(const char *path);
static int is_replayable_name(const char *name);

#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
#define REPO_GUARD_MAX_WATCHES 100000
#define REPO_GUARD_MAX_EXTERNAL_PATHS 1024

typedef struct {
	char root[PATH_BUF];
	int backend_fd;
	int complete;
	int workspace_scan;
	int workspace_registered;
	int workspace_epoch_valid;
	int workspace_need_content;
	int workspace_max_content_files;
	char workspace_tree[HASH_HEX];
	char workspace_content[HASH_HEX];
	int git_context_valid;
	char git_dir[PATH_BUF];
	int git_tool_valid;
	executable_signal git_tool;
	int rg_tool_valid;
	executable_signal rg_tool;
	int git_index_valid;
	char git_index[HASH_HEX + 16];
	int git_config_valid;
	char git_config[HASH_HEX];
	int git_head_valid;
	char git_head[128];
	char git_branch[PATH_BUF];
	int git_ignore_valid;
	char git_ignore[HASH_HEX];
	int git_attributes_valid;
	char git_attributes[HASH_HEX];
	int git_log_view_valid;
	char git_log_view[HASH_HEX];
	int git_object_namespace_valid;
	char git_object_namespace[HASH_HEX];
	int rg_config_valid;
	char rg_config[HASH_HEX];
#if defined(__APPLE__)
	int *watch_fds;
	struct stat *watch_stats;
	size_t watch_count;
	size_t watch_cap;
#endif
	char external_paths[REPO_GUARD_MAX_EXTERNAL_PATHS][PATH_BUF];
	size_t external_count;
} repo_change_guard;

static _Thread_local repo_change_guard *active_repo_guard;

static int repo_guard_path_in_workspace(const repo_change_guard *guard, const char *path) {
	if (guard == NULL || path == NULL || guard->root[0] == '\0') {
		return 0;
	}
	size_t root_len = strlen(guard->root);
	if (strcmp(path, guard->root) == 0) {
		return 1;
	}
	if (strncmp(path, guard->root, root_len) != 0 || path[root_len] != '/') {
		return 0;
	}
	const char *rel = path + root_len + 1;
	return strcmp(rel, ".git") != 0 && strncmp(rel, ".git/", 5) != 0 &&
	       strcmp(rel, ".squire") != 0 && strncmp(rel, ".squire/", 8) != 0;
}

static int repo_guard_parent_path(const char *path, char parent[PATH_BUF]) {
	if (path == NULL || path[0] == '\0' || strlen(path) >= PATH_BUF) {
		return 0;
	}
	snprintf(parent, PATH_BUF, "%s", path);
	char *slash = strrchr(parent, '/');
	if (slash == NULL) {
		snprintf(parent, PATH_BUF, ".");
		return 1;
	}
	if (slash == parent) {
		parent[1] = '\0';
		return 1;
	}
	*slash = '\0';
	return 1;
}

static int repo_guard_existing_parent(const char *path, char parent[PATH_BUF]) {
	if (!repo_guard_parent_path(path, parent)) {
		return 0;
	}
	for (;;) {
		struct stat st;
		if (lstat(parent, &st) == 0 && S_ISDIR(st.st_mode)) {
			return 1;
		}
		char next[PATH_BUF];
		if (!repo_guard_parent_path(parent, next) || strcmp(next, parent) == 0) {
			return 0;
		}
		snprintf(parent, PATH_BUF, "%s", next);
	}
}

static int repo_guard_init(repo_change_guard *guard, const char *root) {
	if (guard == NULL || root == NULL || root[0] == '\0') {
		return 0;
	}
	memset(guard, 0, sizeof(*guard));
	guard->backend_fd = -1;
	snprintf(guard->root, sizeof(guard->root), "%s", root);
#if defined(__APPLE__)
	guard->backend_fd = kqueue();
#else
	guard->backend_fd = inotify_init1(IN_NONBLOCK | IN_CLOEXEC);
#endif
	guard->complete = guard->backend_fd >= 0;
	return guard->complete;
}

static void repo_guard_release(repo_change_guard *guard) {
	if (guard == NULL) {
		return;
	}
	if (guard->backend_fd >= 0) {
		close(guard->backend_fd);
	}
#if defined(__APPLE__)
	for (size_t i = 0; i < guard->watch_count; i++) {
		if (guard->watch_fds[i] >= 0) {
			close(guard->watch_fds[i]);
		}
	}
	free(guard->watch_fds);
	free(guard->watch_stats);
#endif
	memset(guard, 0, sizeof(*guard));
	guard->backend_fd = -1;
}

#if defined(__APPLE__)
static int repo_guard_watch_path(repo_change_guard *guard, const char *path) {
	if (guard == NULL || !guard->complete || path == NULL || path[0] == '\0' || guard->watch_count >= REPO_GUARD_MAX_WATCHES) {
		if (guard != NULL) {
			guard->complete = 0;
		}
		return 0;
	}
	int flags = O_EVTONLY | O_CLOEXEC;
#if defined(O_SYMLINK)
	struct stat lst;
	if (lstat(path, &lst) == 0 && S_ISLNK(lst.st_mode)) {
		flags |= O_SYMLINK;
	}
#endif
	int fd = open(path, flags);
	if (fd < 0) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: repo-guard-open-failed path=%s errno=%d\n", path, errno);
		}
		guard->complete = 0;
		return 0;
	}
	struct stat baseline;
	if (fstat(fd, &baseline) != 0) {
		close(fd);
		guard->complete = 0;
		return 0;
	}
	if (guard->watch_count == guard->watch_cap) {
		size_t next_cap = guard->watch_cap == 0 ? 256 : guard->watch_cap * 2;
		if (next_cap > REPO_GUARD_MAX_WATCHES) {
			next_cap = REPO_GUARD_MAX_WATCHES;
		}
		int *next_fds = (int *)realloc(guard->watch_fds, next_cap * sizeof(int));
		if (next_fds == NULL) {
			close(fd);
			guard->complete = 0;
			return 0;
		}
		guard->watch_fds = next_fds;
		struct stat *next_stats = (struct stat *)realloc(guard->watch_stats, next_cap * sizeof(struct stat));
		if (next_stats == NULL) {
			close(fd);
			guard->complete = 0;
			return 0;
		}
		guard->watch_stats = next_stats;
		guard->watch_cap = next_cap;
	}
	struct kevent change;
	EV_SET(&change, (uintptr_t)fd, EVFILT_VNODE, EV_ADD | EV_CLEAR,
	       NOTE_DELETE | NOTE_WRITE | NOTE_EXTEND | NOTE_ATTRIB | NOTE_LINK | NOTE_RENAME | NOTE_REVOKE,
	       0, (void *)(uintptr_t)(guard->watch_count + 1));
	if (kevent(guard->backend_fd, &change, 1, NULL, 0, NULL) != 0) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
			fprintf(stderr, "squire mmap proof debug: repo-guard-register-failed path=%s errno=%d\n", path, errno);
		}
		close(fd);
		guard->complete = 0;
		return 0;
	}
	guard->watch_fds[guard->watch_count] = fd;
	guard->watch_stats[guard->watch_count] = baseline;
	guard->watch_count++;
	return 1;
}
#else
static int repo_guard_watch_path(repo_change_guard *guard, const char *path) {
	if (guard == NULL || !guard->complete || path == NULL || path[0] == '\0') {
		return 0;
	}
	uint32_t mask = IN_ATTRIB | IN_CLOSE_WRITE | IN_CREATE | IN_DELETE | IN_DELETE_SELF |
	                IN_MODIFY | IN_MOVE_SELF | IN_MOVED_FROM | IN_MOVED_TO | IN_UNMOUNT;
	if (inotify_add_watch(guard->backend_fd, path, mask) < 0) {
		guard->complete = 0;
		return 0;
	}
	return 1;
}
#endif

static int repo_guard_watch_workspace_path(repo_change_guard *guard, const char *path, int is_dir) {
#if defined(__APPLE__)
	(void)is_dir;
	return repo_guard_watch_path(guard, path);
#else
	if (!is_dir) {
		return 1;
	}
	return repo_guard_watch_path(guard, path);
#endif
}

static int repo_guard_external_seen(repo_change_guard *guard, const char *path) {
	for (size_t i = 0; i < guard->external_count; i++) {
		if (strcmp(guard->external_paths[i], path) == 0) {
			return 1;
		}
	}
	return 0;
}

static void repo_guard_watch_dependency(const char *path) {
	repo_change_guard *guard = active_repo_guard;
	if (guard == NULL || !guard->complete || path == NULL || path[0] == '\0') {
		return;
	}
	if ((guard->workspace_scan || guard->workspace_registered) && repo_guard_path_in_workspace(guard, path)) {
		return;
	}
	char watched[PATH_BUF];
	struct stat st;
#if defined(__APPLE__)
	if (lstat(path, &st) == 0) {
		snprintf(watched, sizeof(watched), "%s", path);
	} else if (!repo_guard_existing_parent(path, watched)) {
		guard->complete = 0;
		return;
	}
#else
	if (lstat(path, &st) == 0 && S_ISDIR(st.st_mode)) {
		snprintf(watched, sizeof(watched), "%s", path);
	} else if (!repo_guard_existing_parent(path, watched)) {
		guard->complete = 0;
		return;
	}
#endif
	if (repo_guard_external_seen(guard, watched)) {
		return;
	}
	if (guard->external_count >= REPO_GUARD_MAX_EXTERNAL_PATHS || !repo_guard_watch_path(guard, watched)) {
		guard->complete = 0;
		return;
	}
	snprintf(guard->external_paths[guard->external_count++], PATH_BUF, "%s", watched);
}

static int repo_guard_drain_clean(repo_change_guard *guard) {
	if (guard == NULL || !guard->complete || guard->backend_fd < 0) {
		return 0;
	}
#if defined(__APPLE__)
	struct kevent events[32];
	struct timespec timeout = {0, 0};
	int dirty = 0;
	for (;;) {
		int count = kevent(guard->backend_fd, NULL, 0, events, 32, &timeout);
		if (count < 0) {
			if (errno == EINTR) {
				continue;
			}
			guard->complete = 0;
			return 0;
		}
		if (count == 0) {
			return !dirty;
		}
		for (int i = 0; i < count; i++) {
			unsigned int flags = (unsigned int)events[i].fflags;
			if ((flags & (NOTE_DELETE | NOTE_RENAME | NOTE_REVOKE)) != 0) {
				dirty = 1;
				continue;
			}
			size_t encoded = (size_t)(uintptr_t)events[i].udata;
			if (encoded == 0 || encoded > guard->watch_count || flags == 0) {
				dirty = 1;
				continue;
			}
			size_t index = encoded - 1;
			struct stat current;
			const struct stat *baseline = &guard->watch_stats[index];
			if (fstat(guard->watch_fds[index], &current) != 0 ||
			    current.st_dev != baseline->st_dev || current.st_ino != baseline->st_ino ||
			    current.st_mode != baseline->st_mode || current.st_nlink != baseline->st_nlink ||
			    current.st_uid != baseline->st_uid || current.st_gid != baseline->st_gid ||
			    current.st_size != baseline->st_size ||
			    stat_mtime_nano(&current) != stat_mtime_nano(baseline) ||
			    stat_ctime_nano(&current) != stat_ctime_nano(baseline)) {
				dirty = 1;
			}
		}
	}
#else
	unsigned char events[4096];
	int dirty = 0;
	for (;;) {
		ssize_t count = read(guard->backend_fd, events, sizeof(events));
		if (count > 0) {
			dirty = 1;
			continue;
		}
		if (count < 0 && errno == EINTR) {
			continue;
		}
		if (count < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
			return !dirty;
		}
		guard->complete = 0;
		return 0;
	}
#endif
}
#else
typedef struct {
	int unused;
} repo_change_guard;

static void repo_guard_watch_dependency(const char *path) {
	(void)path;
}
#endif

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

#define HOT_EVENT_CACHE_SLOTS 4
#define HOT_EVENT_FILE_UNAVAILABLE (-1)
#define HOT_EVENT_FILE_FULL (-2)

typedef struct {
	char path[PATH_BUF];
	int fd;
	off_t bytes;
} hot_event_cache_entry;

static hot_event_cache_entry hot_event_cache[HOT_EVENT_CACHE_SLOTS];
static pthread_mutex_t hot_event_cache_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_once_t hot_event_cache_once = PTHREAD_ONCE_INIT;
static _Thread_local hot_event_cache_entry *hot_event_thread_entry;

static void hot_event_cache_init(void) {
	for (size_t i = 0; i < HOT_EVENT_CACHE_SLOTS; i++) {
		hot_event_cache[i].fd = -1;
	}
}

static int cached_hot_event_file(const char *path) {
	if (path == NULL || path[0] == '\0') {
		return -1;
	}
	pthread_once(&hot_event_cache_once, hot_event_cache_init);
	if (hot_event_thread_entry != NULL && hot_event_thread_entry->fd >= 0 &&
	    strcmp(hot_event_thread_entry->path, path) == 0) {
		off_t bytes = __atomic_load_n(&hot_event_thread_entry->bytes, __ATOMIC_RELAXED);
		return bytes < HOT_CLIENT_STATS_MAX_BYTES ? hot_event_thread_entry->fd : HOT_EVENT_FILE_FULL;
	}
	pthread_mutex_lock(&hot_event_cache_mu);
	for (size_t i = 0; i < HOT_EVENT_CACHE_SLOTS; i++) {
		if (hot_event_cache[i].fd < 0 || strcmp(hot_event_cache[i].path, path) != 0) {
			continue;
		}
		int fd = hot_event_cache[i].fd;
		if (__atomic_load_n(&hot_event_cache[i].bytes, __ATOMIC_RELAXED) >= HOT_CLIENT_STATS_MAX_BYTES) {
			pthread_mutex_unlock(&hot_event_cache_mu);
			return HOT_EVENT_FILE_FULL;
		}
		hot_event_thread_entry = &hot_event_cache[i];
		pthread_mutex_unlock(&hot_event_cache_mu);
		return fd;
	}
	for (size_t i = 0; i < HOT_EVENT_CACHE_SLOTS; i++) {
		if (hot_event_cache[i].fd >= 0) {
			continue;
		}
		int fd = open(path, O_CREAT | O_WRONLY | O_APPEND, 0600);
		if (fd < 0) {
			pthread_mutex_unlock(&hot_event_cache_mu);
			return -1;
		}
		struct stat st;
		if (fstat(fd, &st) != 0) {
			close(fd);
			pthread_mutex_unlock(&hot_event_cache_mu);
			return HOT_EVENT_FILE_UNAVAILABLE;
		}
		if (st.st_size >= HOT_CLIENT_STATS_MAX_BYTES) {
			close(fd);
			pthread_mutex_unlock(&hot_event_cache_mu);
			return HOT_EVENT_FILE_FULL;
		}
		hot_event_cache[i].fd = fd;
		hot_event_cache[i].bytes = st.st_size;
		snprintf(hot_event_cache[i].path, sizeof(hot_event_cache[i].path), "%s", path);
		hot_event_thread_entry = &hot_event_cache[i];
		pthread_mutex_unlock(&hot_event_cache_mu);
		return fd;
	}
	pthread_mutex_unlock(&hot_event_cache_mu);
	return -1;
}

static void note_cached_hot_event_write(int fd, size_t bytes) {
	if (hot_event_thread_entry != NULL && hot_event_thread_entry->fd == fd) {
		(void)__atomic_add_fetch(&hot_event_thread_entry->bytes, (off_t)bytes, __ATOMIC_RELAXED);
		return;
	}
	pthread_mutex_lock(&hot_event_cache_mu);
	for (size_t i = 0; i < HOT_EVENT_CACHE_SLOTS; i++) {
		if (hot_event_cache[i].fd == fd) {
			(void)__atomic_add_fetch(&hot_event_cache[i].bytes, (off_t)bytes, __ATOMIC_RELAXED);
			hot_event_thread_entry = &hot_event_cache[i];
			break;
		}
	}
	pthread_mutex_unlock(&hot_event_cache_mu);
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
	char event_path[PATH_BUF];
	if (!join_path(event_path, sizeof(event_path), store_root, "hot_client_events.log")) {
		mmap_trace_path("event-write-skip-path", store_root);
		return;
	}
	int fd = cached_hot_event_file(event_path);
	if (fd == HOT_EVENT_FILE_UNAVAILABLE && mkdir_p(store_root)) {
		fd = cached_hot_event_file(event_path);
	}
	if (fd < 0) {
		mmap_trace_errno_path("event-write-skip-open", event_path, errno);
		return;
	}
	if (write_all(fd, line, (size_t)n)) {
		note_cached_hot_event_write(fd, (size_t)n);
		mmap_trace_path("event-write-ok", event_path);
	} else {
		mmap_trace_errno_path("event-write-skip-write", event_path, errno);
	}
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
	(void)argc;
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

static void sha256_hex_join(const char *const *parts, size_t count, char out[HASH_HEX]) {
	unsigned char digest[32];
	static const char hex[] = "0123456789abcdef";
	static const unsigned char separator = '|';
	SQUIRE_SHA256_CTX ctx;
	SQUIRE_SHA256_Init(&ctx);
	for (size_t i = 0; i < count; i++) {
		if (i > 0) {
			SQUIRE_SHA256_Update(&ctx, &separator, 1);
		}
		SQUIRE_SHA256_Update(&ctx, parts[i], strlen(parts[i]));
	}
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
	repo_guard_watch_dependency(path);
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
	repo_guard_watch_dependency(path);
	int fd = open(path, O_RDONLY);
	if (fd < 0) {
		return 0;
	}
	struct stat st;
	if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size < 0 ||
	    (content != NULL && st.st_size > MAX_WARM_FILE_BYTES)) {
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

#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
#define HOT_FILE_CACHE_SLOTS 64

typedef struct {
	char path[PATH_BUF];
	struct stat st;
	char content_hash[HASH_HEX];
	unsigned char *content;
	size_t content_len;
	unsigned long long last_used;
	int occupied;
	repo_change_guard guard;
} hot_file_cache_entry;

static hot_file_cache_entry hot_file_cache[HOT_FILE_CACHE_SLOTS];
static pthread_mutex_t hot_file_cache_mu = PTHREAD_MUTEX_INITIALIZER;
static unsigned long long hot_file_cache_clock;

static int same_file_identity(const struct stat *left, const struct stat *right) {
	return left->st_dev == right->st_dev && left->st_ino == right->st_ino &&
	       left->st_mode == right->st_mode && left->st_size == right->st_size &&
	       stat_mtime_nano(left) == stat_mtime_nano(right) &&
	       stat_ctime_nano(left) == stat_ctime_nano(right);
}

static void hot_file_cache_clear(hot_file_cache_entry *entry) {
	if (entry == NULL) {
		return;
	}
	if (entry->occupied || entry->guard.root[0] != '\0') {
		repo_guard_release(&entry->guard);
	}
	free(entry->content);
	memset(entry, 0, sizeof(*entry));
	entry->guard.backend_fd = -1;
}

static int hot_file_cache_copy(const hot_file_cache_entry *entry, struct stat *st, char content_hash[HASH_HEX],
	                           unsigned char **content, size_t *content_len) {
	if (entry == NULL || !entry->occupied) {
		return 0;
	}
	if (st != NULL) {
		*st = entry->st;
	}
	memcpy(content_hash, entry->content_hash, HASH_HEX);
	if (content != NULL) {
		unsigned char *copy = NULL;
		if (entry->content_len > 0) {
			copy = (unsigned char *)malloc(entry->content_len);
			if (copy == NULL) {
				return 0;
			}
			memcpy(copy, entry->content, entry->content_len);
		}
		*content = copy;
		if (content_len != NULL) {
			*content_len = entry->content_len;
		}
	}
	return 1;
}

static int hot_file_read_proven(const char *path, struct stat *st, char content_hash[HASH_HEX],
	                            unsigned char **content, size_t *content_len) {
	if (content != NULL) {
		*content = NULL;
	}
	if (content_len != NULL) {
		*content_len = 0;
	}
	pthread_mutex_lock(&hot_file_cache_mu);
	for (size_t i = 0; i < HOT_FILE_CACHE_SLOTS; i++) {
		hot_file_cache_entry *entry = &hot_file_cache[i];
		if (!entry->occupied || strcmp(entry->path, path) != 0) {
			continue;
		}
		struct stat current;
		if (!repo_guard_drain_clean(&entry->guard) || stat(path, &current) != 0 || !same_file_identity(&entry->st, &current)) {
			hot_file_cache_clear(entry);
			break;
		}
		entry->last_used = ++hot_file_cache_clock;
		int ok = hot_file_cache_copy(entry, st, content_hash, content, content_len);
		pthread_mutex_unlock(&hot_file_cache_mu);
		return ok;
	}

	hot_file_cache_entry *selected = NULL;
	for (size_t i = 0; i < HOT_FILE_CACHE_SLOTS; i++) {
		hot_file_cache_entry *entry = &hot_file_cache[i];
		if (!entry->occupied) {
			selected = entry;
			break;
		}
		if (selected == NULL || entry->last_used < selected->last_used) {
			selected = entry;
		}
	}
	if (selected == NULL) {
		pthread_mutex_unlock(&hot_file_cache_mu);
		return 0;
	}

	for (int attempt = 0; attempt < 2; attempt++) {
		hot_file_cache_clear(selected);
		char parent[PATH_BUF];
		if (!repo_guard_parent_path(path, parent) || !repo_guard_init(&selected->guard, parent)) {
			break;
		}
		struct stat before, after;
		unsigned char *loaded = NULL;
		size_t loaded_len = 0;
		active_repo_guard = &selected->guard;
		repo_guard_watch_dependency(parent);
		int ok = stat(path, &before) == 0 && S_ISREG(before.st_mode) && before.st_size >= 0 && before.st_size <= MAX_WARM_FILE_BYTES &&
		         read_file_hash(path, selected->content_hash, &loaded, &loaded_len) &&
		         stat(path, &after) == 0 && same_file_identity(&before, &after);
		active_repo_guard = NULL;
		if (!ok || loaded_len != (size_t)before.st_size || !repo_guard_drain_clean(&selected->guard)) {
			free(loaded);
			continue;
		}
		selected->st = after;
		selected->content = loaded;
		selected->content_len = loaded_len;
		selected->last_used = ++hot_file_cache_clock;
		selected->occupied = 1;
		snprintf(selected->path, sizeof(selected->path), "%s", path);
		ok = hot_file_cache_copy(selected, st, content_hash, content, content_len);
		pthread_mutex_unlock(&hot_file_cache_mu);
		return ok;
	}
	hot_file_cache_clear(selected);
	pthread_mutex_unlock(&hot_file_cache_mu);
	return 0;
}
#else
static int hot_file_read_proven(const char *path, struct stat *st, char content_hash[HASH_HEX],
	                            unsigned char **content, size_t *content_len) {
	if (stat(path, st) != 0 || !S_ISREG(st->st_mode) || st->st_size < 0 || st->st_size > MAX_WARM_FILE_BYTES) {
		return 0;
	}
	return read_file_hash(path, content_hash, content, content_len);
}
#endif

static int read_executable_hash(const char *path, char out[HASH_HEX]) {
	repo_guard_watch_dependency(path);
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

#define EXECUTABLE_SIGNAL_CACHE_SLOTS 64

typedef struct {
	char path[PATH_BUF];
	struct stat st;
	executable_signal signal;
	unsigned long long last_used;
	int occupied;
} executable_signal_cache_entry;

static executable_signal_cache_entry executable_signal_cache[EXECUTABLE_SIGNAL_CACHE_SLOTS];
static pthread_mutex_t executable_signal_cache_mu = PTHREAD_MUTEX_INITIALIZER;
static unsigned long long executable_signal_cache_clock;

static int executable_signal_identity_matches(const struct stat *left, const struct stat *right) {
	return left->st_dev == right->st_dev && left->st_ino == right->st_ino &&
	       left->st_mode == right->st_mode && left->st_size == right->st_size &&
	       stat_mtime_nano(left) == stat_mtime_nano(right) &&
	       stat_ctime_nano(left) == stat_ctime_nano(right);
}

static int executable_signal_cache_lookup(const char *path, const struct stat *st, executable_signal *sig) {
	pthread_mutex_lock(&executable_signal_cache_mu);
	for (size_t i = 0; i < EXECUTABLE_SIGNAL_CACHE_SLOTS; i++) {
		executable_signal_cache_entry *entry = &executable_signal_cache[i];
		if (!entry->occupied || strcmp(entry->path, path) != 0 ||
		    !executable_signal_identity_matches(&entry->st, st)) {
			continue;
		}
		entry->last_used = ++executable_signal_cache_clock;
		*sig = entry->signal;
		pthread_mutex_unlock(&executable_signal_cache_mu);
		return 1;
	}
	pthread_mutex_unlock(&executable_signal_cache_mu);
	return 0;
}

static void executable_signal_cache_store(const char *path, const struct stat *st, const executable_signal *sig) {
	pthread_mutex_lock(&executable_signal_cache_mu);
	executable_signal_cache_entry *selected = NULL;
	for (size_t i = 0; i < EXECUTABLE_SIGNAL_CACHE_SLOTS; i++) {
		executable_signal_cache_entry *entry = &executable_signal_cache[i];
		if (entry->occupied && strcmp(entry->path, path) == 0) {
			selected = entry;
			break;
		}
		if (!entry->occupied) {
			selected = entry;
			break;
		}
		if (selected == NULL || entry->last_used < selected->last_used) {
			selected = entry;
		}
	}
	if (selected != NULL) {
		memset(selected, 0, sizeof(*selected));
		selected->occupied = 1;
		snprintf(selected->path, sizeof(selected->path), "%s", path);
		selected->st = *st;
		selected->signal = *sig;
		selected->last_used = ++executable_signal_cache_clock;
	}
	pthread_mutex_unlock(&executable_signal_cache_mu);
}

static int executable_signal_for(const char *cwd, const char *name, executable_signal *sig) {
	char path[PATH_BUF];
	if (!resolve_executable(cwd, name, path)) {
		return 0;
	}
	repo_guard_watch_dependency(path);
	struct stat st;
	if (stat(path, &st) != 0 || S_ISDIR(st.st_mode) || (st.st_mode & 0111) == 0) {
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL && strcmp(name, "file") == 0) {
			fprintf(stderr, "squire mmap proof debug: executable file stat reject path=%s errno=%d\n", path, errno);
		}
		return 0;
	}
	if (executable_signal_cache_lookup(path, &st, sig)) {
		return 1;
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
		executable_signal_cache_store(path, &st, sig);
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
	executable_signal_cache_store(path, &st, sig);
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

static int ripgrep_config_fingerprint(char out[HASH_HEX]) {
	const char *path = getenv("RIPGREP_CONFIG_PATH");
	if (path == NULL) {
		path = "";
	}
	char fp[HASH_HEX + 16];
	file_hash_or_missing(path, fp);
	byte_buf b = {0};
	int ok = bytes_append_str(&b, path) && bytes_append_byte(&b, 0) && bytes_append_str(&b, fp);
	if (ok) {
		sha256_hex_buf(&b, out);
	}
	bytes_free(&b);
	return ok;
}

static int ripgrep_environment_fingerprint(char out[HASH_HEX]) {
	static const char *keys[] = {
		"RIPGREP_CONFIG_PATH", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "TERM",
		"COLORTERM", "LANG", "LC_ALL", "LC_CTYPE", "GREP_COLORS",
	};
	return hash_selected_environment(keys, sizeof(keys) / sizeof(keys[0]), out);
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

static int collect_tree_file_fingerprints(const char *dir, string_list *parts) {
	repo_guard_watch_dependency(dir);
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
			if (!collect_tree_file_fingerprints(path, parts)) {
				closedir(d);
				return 0;
			}
			continue;
		}
		char fp[HASH_HEX + 16];
		file_hash_or_missing(path, fp);
		if (!list_add_path_hash_part(parts, path, fp)) {
			closedir(d);
			return 0;
		}
	}
	closedir(d);
	return 1;
}

static int git_log_view_fingerprint(const char *git_dir, char out[HASH_HEX]) {
	string_list parts = {0};
	char path[PATH_BUF], fp[HASH_HEX + 16];
	static const char *direct_paths[] = {"packed-refs", "info/grafts", "shallow", "refs"};
	for (size_t i = 0; i < sizeof(direct_paths) / sizeof(direct_paths[0]); i++) {
		if (!join_path(path, sizeof(path), git_dir, direct_paths[i])) {
			list_free(&parts);
			return 0;
		}
		file_hash_or_missing(path, fp);
		if (!list_add_path_hash_part(&parts, path, fp)) {
			list_free(&parts);
			return 0;
		}
	}
	if (!join_path(path, sizeof(path), git_dir, "refs") ||
	    !collect_tree_file_fingerprints(path, &parts)) {
		list_free(&parts);
		return 0;
	}
	int ok = hash_joined_lines(&parts, out);
	list_free(&parts);
	return ok;
}

static int lower_hex_n(const char *value, size_t len) {
	if (value == NULL || len == 0 || strlen(value) != len) {
		return 0;
	}
	for (size_t i = 0; i < len; i++) {
		if (!((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f'))) {
			return 0;
		}
	}
	return 1;
}

static int git_history_standard_layout(const char *git_dir) {
	static const char *keys[] = {
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_COMMON_DIR",
		"GIT_NAMESPACE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NO_REPLACE_OBJECTS",
		"GIT_REPLACE_REF_BASE",
		"GIT_SHALLOW_FILE",
		"GIT_LITERAL_PATHSPECS",
		"GIT_GLOB_PATHSPECS",
		"GIT_NOGLOB_PATHSPECS",
		"GIT_ICASE_PATHSPECS",
		"GIT_CEILING_DIRECTORIES",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM",
	};
	for (size_t i = 0; i < sizeof(keys) / sizeof(keys[0]); i++) {
		const char *value = getenv(keys[i]);
		if (value != NULL && value[0] != '\0') {
			return 0;
		}
	}
	char path[PATH_BUF], text[PATH_BUF];
	static const char *layout_files[] = {"commondir", "objects/info/alternates"};
	for (size_t i = 0; i < sizeof(layout_files) / sizeof(layout_files[0]); i++) {
		if (!join_path(path, sizeof(path), git_dir, layout_files[i])) {
			return 0;
		}
		repo_guard_watch_dependency(path);
		struct stat st;
		if (lstat(path, &st) == 0) {
			if (!S_ISREG(st.st_mode)) {
				return 0;
			}
			if (st.st_size > 0 && (!read_file_trimmed(path, text, sizeof(text)) || text[0] != '\0')) {
				return 0;
			}
		} else if (errno != ENOENT) {
			return 0;
		}
	}
	return 1;
}

static int git_object_namespace_fingerprint(const char *git_dir, char out[HASH_HEX]) {
	if (!git_history_standard_layout(git_dir)) {
		return 0;
	}
	char objects_dir[PATH_BUF];
	if (!join_path(objects_dir, sizeof(objects_dir), git_dir, "objects")) {
		return 0;
	}
	repo_guard_watch_dependency(objects_dir);
	struct stat objects_st;
	if (lstat(objects_dir, &objects_st) != 0 || !S_ISDIR(objects_st.st_mode)) {
		return 0;
	}
	DIR *objects = opendir(objects_dir);
	if (objects == NULL) {
		return 0;
	}
	string_list parts = {0};
	static const char format[] = "format:sha1";
	int ok = list_add_bytes(&parts, (const unsigned char *)format, strlen(format));
	struct dirent *entry;
	while (ok && (entry = readdir(objects)) != NULL) {
		if (strcmp(entry->d_name, ".") == 0 || strcmp(entry->d_name, "..") == 0) {
			continue;
		}
		char path[PATH_BUF];
		if (!join_path(path, sizeof(path), objects_dir, entry->d_name)) {
			ok = 0;
			break;
		}
		if (lower_hex_n(entry->d_name, 2)) {
			repo_guard_watch_dependency(path);
			DIR *loose = opendir(path);
			if (loose == NULL) {
				ok = 0;
				break;
			}
			struct dirent *object;
			while (ok && (object = readdir(loose)) != NULL) {
				if (!lower_hex_n(object->d_name, 38)) {
					continue;
				}
				char object_path[PATH_BUF];
				struct stat object_st;
				if (!join_path(object_path, sizeof(object_path), path, object->d_name) ||
				    lstat(object_path, &object_st) != 0) {
					ok = 0;
					break;
				}
				if (!S_ISREG(object_st.st_mode)) {
					continue;
				}
				char label[64];
				int n = snprintf(label, sizeof(label), "loose:%s%s", entry->d_name, object->d_name);
				if (n <= 0 || (size_t)n >= sizeof(label) ||
				    !list_add_bytes(&parts, (const unsigned char *)label, (size_t)n)) {
					ok = 0;
				}
			}
			closedir(loose);
			continue;
		}
		if (strcmp(entry->d_name, "pack") != 0) {
			continue;
		}
		repo_guard_watch_dependency(path);
		DIR *pack = opendir(path);
		if (pack == NULL) {
			ok = 0;
			break;
		}
		struct dirent *item;
		while (ok && (item = readdir(pack)) != NULL) {
			size_t name_len = strlen(item->d_name);
			int is_index = name_len > 4 && strcmp(item->d_name + name_len - 4, ".idx") == 0;
			int is_multi = strcmp(item->d_name, "multi-pack-index") == 0;
			if (!is_index && !is_multi) {
				continue;
			}
			char item_path[PATH_BUF], fp[HASH_HEX + 16], label[PATH_BUF];
			struct stat item_st;
			if (!join_path(item_path, sizeof(item_path), path, item->d_name) ||
			    lstat(item_path, &item_st) != 0 || !S_ISREG(item_st.st_mode)) {
				ok = 0;
				break;
			}
			file_hash_or_missing(item_path, fp);
			if (is_multi) {
				snprintf(label, sizeof(label), "multi-pack-index");
			} else if (snprintf(label, sizeof(label), "pack-index:%s", item->d_name) >= (int)sizeof(label)) {
				ok = 0;
				break;
			}
			if (!list_add_path_hash_part(&parts, label, fp)) {
				ok = 0;
			}
		}
		closedir(pack);
	}
	closedir(objects);
	if (ok) {
		ok = hash_joined_lines(&parts, out);
	}
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

static long long stat_ctime_nano(const struct stat *st) {
#if defined(__APPLE__)
	return (long long)st->st_ctimespec.tv_sec * 1000000000LL + (long long)st->st_ctimespec.tv_nsec;
#elif defined(__linux__)
	return (long long)st->st_ctim.tv_sec * 1000000000LL + (long long)st->st_ctim.tv_nsec;
#else
	return (long long)st->st_ctime * 1000000000LL;
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
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->workspace_scan) {
		(void)repo_guard_watch_workspace_path(active_repo_guard, dir, 1);
	}
#endif
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
#if defined(SQUIRE_MMAP_HOT_API) && defined(__APPLE__)
		if (active_repo_guard != NULL && active_repo_guard->workspace_scan && S_ISREG(st.st_mode)) {
			(void)repo_guard_watch_workspace_path(active_repo_guard, path, 0);
		}
#endif
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
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->workspace_epoch_valid &&
	    strcmp(active_repo_guard->root, root) == 0 &&
	    active_repo_guard->workspace_need_content == need_content &&
	    active_repo_guard->workspace_max_content_files == max_content_files) {
		snprintf(tree, HASH_HEX, "%s", active_repo_guard->workspace_tree);
		snprintf(content, HASH_HEX, "%s", active_repo_guard->workspace_content);
		return 1;
	}
#endif
	workspace_epoch_builder b = {0};
	b.root = root;
	b.need_content = need_content;
	b.max_content_files = max_content_files;
	b.complete = 1;
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->workspace_scan = !active_repo_guard->workspace_registered;
	}
#endif
	if (!collect_workspace_epochs(&b, root)) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
		if (active_repo_guard != NULL) {
			active_repo_guard->workspace_scan = 0;
		}
#endif
		list_free(&b.tree);
		list_free(&b.content);
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->workspace_scan = 0;
		active_repo_guard->workspace_registered = active_repo_guard->complete;
	}
#endif
	int ok = b.complete && hash_joined_lines(&b.tree, tree) && hash_joined_lines(&b.content, content);
	list_free(&b.tree);
	list_free(&b.content);
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (ok && active_repo_guard != NULL && active_repo_guard->complete &&
	    strcmp(active_repo_guard->root, root) == 0) {
		active_repo_guard->workspace_epoch_valid = 1;
		active_repo_guard->workspace_need_content = need_content;
		active_repo_guard->workspace_max_content_files = max_content_files;
		snprintf(active_repo_guard->workspace_tree, HASH_HEX, "%s", tree);
		snprintf(active_repo_guard->workspace_content, HASH_HEX, "%s", content);
	}
#endif
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

static int guarded_git_context(policy_invocation *inv, char repo_root[PATH_BUF], char git_dir[PATH_BUF]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->git_context_valid) {
		snprintf(repo_root, PATH_BUF, "%s", active_repo_guard->root);
		snprintf(git_dir, PATH_BUF, "%s", active_repo_guard->git_dir);
		return 1;
	}
#endif
	if (!discover_git_dir(inv->cwd, repo_root, git_dir)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && strcmp(active_repo_guard->root, repo_root) == 0) {
		active_repo_guard->git_context_valid = 1;
		snprintf(active_repo_guard->git_dir, PATH_BUF, "%s", git_dir);
	}
#endif
	return 1;
}

static int guarded_executable_signal(const char *cwd, const char *name, executable_signal *sig) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		if (strcmp(name, "git") == 0 && active_repo_guard->git_tool_valid) {
			*sig = active_repo_guard->git_tool;
			return 1;
		}
		if (strcmp(name, "rg") == 0 && active_repo_guard->rg_tool_valid) {
			*sig = active_repo_guard->rg_tool;
			return 1;
		}
	}
#endif
	if (!executable_signal_for(cwd, name, sig)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		if (strcmp(name, "git") == 0) {
			active_repo_guard->git_tool = *sig;
			active_repo_guard->git_tool_valid = 1;
		} else if (strcmp(name, "rg") == 0) {
			active_repo_guard->rg_tool = *sig;
			active_repo_guard->rg_tool_valid = 1;
		}
	}
#endif
	return 1;
}

static int guarded_git_index_fingerprint(const char *git_dir, char out[HASH_HEX + 16]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->git_index_valid) {
		snprintf(out, HASH_HEX + 16, "%s", active_repo_guard->git_index);
		return 1;
	}
#endif
	char index_path[PATH_BUF];
	if (!join_path(index_path, sizeof(index_path), git_dir, "index")) {
		return 0;
	}
	file_hash_or_missing(index_path, out);
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->git_index_valid = 1;
		snprintf(active_repo_guard->git_index, sizeof(active_repo_guard->git_index), "%s", out);
	}
#endif
	return 1;
}

static int guarded_git_config_fingerprint(const char *repo_root, const char *git_dir, char out[HASH_HEX]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->git_config_valid) {
		snprintf(out, HASH_HEX, "%s", active_repo_guard->git_config);
		return 1;
	}
#endif
	if (!git_config_summary_fingerprint(repo_root, git_dir, out)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->git_config_valid = 1;
		snprintf(active_repo_guard->git_config, HASH_HEX, "%s", out);
	}
#endif
	return 1;
}

static int guarded_git_head(const char *git_dir, char head[128], char branch[PATH_BUF]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->git_head_valid) {
		snprintf(head, 128, "%s", active_repo_guard->git_head);
		snprintf(branch, PATH_BUF, "%s", active_repo_guard->git_branch);
		return 1;
	}
#endif
	if (!current_head_and_branch(git_dir, head, branch)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->git_head_valid = 1;
		snprintf(active_repo_guard->git_head, sizeof(active_repo_guard->git_head), "%s", head);
		snprintf(active_repo_guard->git_branch, sizeof(active_repo_guard->git_branch), "%s", branch);
	}
#endif
	return 1;
}

static int guarded_git_ignore_fingerprint(const char *repo_root, const char *git_dir, char out[HASH_HEX]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->git_ignore_valid) {
		snprintf(out, HASH_HEX, "%s", active_repo_guard->git_ignore);
		return 1;
	}
#endif
	if (!workspace_ignore_fingerprint(repo_root, git_dir, out)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->git_ignore_valid = 1;
		snprintf(active_repo_guard->git_ignore, HASH_HEX, "%s", out);
	}
#endif
	return 1;
}

static int guarded_git_attribute_fingerprint(const char *repo_root, const char *git_dir, char out[HASH_HEX]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->git_attributes_valid) {
		snprintf(out, HASH_HEX, "%s", active_repo_guard->git_attributes);
		return 1;
	}
#endif
	if (!git_attribute_fingerprint(repo_root, git_dir, out)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->git_attributes_valid = 1;
		snprintf(active_repo_guard->git_attributes, HASH_HEX, "%s", out);
	}
#endif
	return 1;
}

static int guarded_git_log_view_fingerprint(const char *git_dir, char out[HASH_HEX]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->git_log_view_valid) {
		snprintf(out, HASH_HEX, "%s", active_repo_guard->git_log_view);
		return 1;
	}
#endif
	if (!git_log_view_fingerprint(git_dir, out)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->git_log_view_valid = 1;
		snprintf(active_repo_guard->git_log_view, HASH_HEX, "%s", out);
	}
#endif
	return 1;
}

static int guarded_git_object_namespace_fingerprint(const char *git_dir, char out[HASH_HEX]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->git_object_namespace_valid) {
		snprintf(out, HASH_HEX, "%s", active_repo_guard->git_object_namespace);
		return 1;
	}
#endif
	if (!git_object_namespace_fingerprint(git_dir, out)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->git_object_namespace_valid = 1;
		snprintf(active_repo_guard->git_object_namespace, HASH_HEX, "%s", out);
	}
#endif
	return 1;
}

static int guarded_rg_config_fingerprint(char out[HASH_HEX]) {
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL && active_repo_guard->rg_config_valid) {
		snprintf(out, HASH_HEX, "%s", active_repo_guard->rg_config);
		return 1;
	}
#endif
	if (!ripgrep_config_fingerprint(out)) {
		return 0;
	}
#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
	if (active_repo_guard != NULL) {
		active_repo_guard->rg_config_valid = 1;
		snprintf(active_repo_guard->rg_config, HASH_HEX, "%s", out);
	}
#endif
	return 1;
}

static int git_metadata_epoch_uncached(policy_invocation *inv, char epoch[256]) {
	if (!is_git_metadata(inv)) {
		return 0;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF], head[128], branch[PATH_BUF], git_rel[PATH_BUF];
	if (!guarded_git_context(inv, repo_root, git_dir) || !guarded_git_head(git_dir, head, branch) || !rel_git_dir(inv->cwd, git_dir, git_rel)) {
		return 0;
	}
	char h[HASH_HEX];
	if (inv->argc == 3 && strcmp(inv->argv[2], "HEAD") == 0) {
		const char *parts[] = {repo_root, head};
		sha256_hex_join(parts, 2, h);
		snprintf(epoch, 256, "hot-head:%s", h);
		return 1;
	}
	if (inv->argc == 4 && strcmp(inv->argv[2], "--abbrev-ref") == 0) {
		const char *parts[] = {repo_root, branch, head};
		sha256_hex_join(parts, 3, h);
		snprintf(epoch, 256, "hot-branch:%s", h);
		return 1;
	}
	if (inv->argc == 3 && strcmp(inv->argv[1], "branch") == 0 && strcmp(inv->argv[2], "--show-current") == 0) {
		const char *parts[] = {repo_root, branch, head};
		sha256_hex_join(parts, 3, h);
		snprintf(epoch, 256, "hot-branch:%s", h);
		return 1;
	}
	if (strcmp(inv->argv[2], "--git-dir") == 0) {
		const char *parts[] = {repo_root, git_rel, git_dir};
		sha256_hex_join(parts, 3, h);
		snprintf(epoch, 256, "hot-gitdir:%s", h);
		return 1;
	}
	if (strcmp(inv->argv[2], "--show-toplevel") == 0) {
		const char *parts[] = {repo_root, git_dir};
		sha256_hex_join(parts, 2, h);
		snprintf(epoch, 256, "hot-repo-root:%s", h);
		return 1;
	}
	if (strcmp(inv->argv[2], "--is-inside-work-tree") == 0) {
		const char *parts[] = {repo_root, git_dir};
		sha256_hex_join(parts, 2, h);
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

static int is_git_head_subject_log(policy_invocation *inv) {
	return inv->argc == 4 &&
	       strcmp(inv->argv[0], "git") == 0 &&
	       strcmp(inv->argv[1], "log") == 0 &&
	       strcmp(inv->argv[2], "-1") == 0 &&
	       strcmp(inv->argv[3], "--format=%H%n%s") == 0;
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
	if (inv->argc == 3 && (strcmp(inv->argv[2], "--stat") == 0 || strcmp(inv->argv[2], "--check") == 0)) {
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

static int is_fixed_rg_repo_search(policy_invocation *inv) {
	if (inv->argc < 3 || strcmp(inv->argv[0], "rg") != 0) {
		return 0;
	}
	int fixed = 0;
	int seen_pattern = 0;
	int path_count = 0;
	char only_path[PATH_BUF] = {0};
	for (int i = 1; i < inv->argc; i++) {
		const char *arg = inv->argv[i];
		if (strcmp(arg, "-F") == 0 || strcmp(arg, "--fixed-strings") == 0) {
			if (fixed || seen_pattern) {
				return 0;
			}
			fixed = 1;
			continue;
		}
		if (strcmp(arg, "-q") == 0 || strcmp(arg, "--quiet") == 0 ||
		    strcmp(arg, "-n") == 0 || strcmp(arg, "--line-number") == 0 ||
		    strcmp(arg, "--no-heading") == 0 || strcmp(arg, "--with-filename") == 0 ||
		    strcmp(arg, "--no-filename") == 0 || strcmp(arg, "-l") == 0 ||
		    strcmp(arg, "--files-with-matches") == 0) {
			if (seen_pattern) {
				return 0;
			}
			continue;
		}
		if (arg[0] == '-') {
			return 0;
		}
		if (!seen_pattern) {
			if (arg[0] == '\0' || strchr(arg, '\n') != NULL || strchr(arg, '\r') != NULL) {
				return 0;
			}
			seen_pattern = 1;
			continue;
		}
		if (strcmp(arg, ".") != 0 && !safe_relative_inspection_path_arg(arg)) {
			return 0;
		}
		path_count++;
		if (path_count == 1) {
			snprintf(only_path, sizeof(only_path), "%s", arg);
		}
	}
	if (!fixed || !seen_pattern) {
		return 0;
	}
	if (path_count == 0) {
		return 0;
	}
	if (path_count > 1 || strcmp(only_path, ".") == 0) {
		return 1;
	}
	return !is_replayable_name(base_name(only_path));
}

static int bounded_rg_text(const char *value, size_t max_len) {
	if (value == NULL || value[0] == '\0' || strnlen(value, max_len + 1) > max_len) {
		return 0;
	}
	return strchr(value, '\n') == NULL && strchr(value, '\r') == NULL;
}

static int bounded_rg_context(const char *value) {
	if (!bounded_rg_text(value, 8)) {
		return 0;
	}
	char *end = NULL;
	errno = 0;
	long count = strtol(value, &end, 10);
	return errno == 0 && end != value && end != NULL && *end == '\0' && count >= 0 && count <= 1000;
}

/*
 * This is a preparation policy, not a regex implementation. The native rg
 * process computes exact bytes after a miss; Squire only replays those bytes
 * while the complete workspace, rg binary, config, and relevant environment
 * proof are unchanged.
 */
static int is_bounded_rg_repo_search(policy_invocation *inv) {
	if (inv->argc < 2 || inv->argc > 32 || strcmp(inv->argv[0], "rg") != 0) {
		return 0;
	}
	int seen_pattern = 0;
	int path_count = 0;
	for (int i = 1; i < inv->argc; i++) {
		const char *arg = inv->argv[i];
		if (strcmp(arg, "-n") == 0 || strcmp(arg, "--line-number") == 0 ||
		    strcmp(arg, "-S") == 0 || strcmp(arg, "--smart-case") == 0 ||
		    strcmp(arg, "-i") == 0 || strcmp(arg, "--ignore-case") == 0 ||
		    strcmp(arg, "--hidden") == 0 || strcmp(arg, "--no-heading") == 0 ||
		    strcmp(arg, "--with-filename") == 0 || strcmp(arg, "--no-filename") == 0 ||
		    strcmp(arg, "-l") == 0 || strcmp(arg, "--files-with-matches") == 0 ||
		    strcmp(arg, "-q") == 0 || strcmp(arg, "--quiet") == 0) {
			continue;
		}
		if (strcmp(arg, "-F") == 0 || strcmp(arg, "--fixed-strings") == 0) {
			continue;
		}
		if (strcmp(arg, "-g") == 0 || strcmp(arg, "--glob") == 0) {
			if (++i >= inv->argc || !bounded_rg_text(inv->argv[i], 1024)) {
				return 0;
			}
			continue;
		}
		if (strcmp(arg, "-C") == 0 || strcmp(arg, "--context") == 0 ||
		    strcmp(arg, "-A") == 0 || strcmp(arg, "--after-context") == 0 ||
		    strcmp(arg, "-B") == 0 || strcmp(arg, "--before-context") == 0) {
			if (++i >= inv->argc || !bounded_rg_context(inv->argv[i])) {
				return 0;
			}
			continue;
		}
		if (strncmp(arg, "--glob=", 7) == 0) {
			if (!bounded_rg_text(arg + 7, 1024)) {
				return 0;
			}
			continue;
		}
		static const char *context_prefixes[] = {"--context=", "--after-context=", "--before-context="};
		int matched_context = 0;
		for (size_t j = 0; j < sizeof(context_prefixes) / sizeof(context_prefixes[0]); j++) {
			size_t prefix_len = strlen(context_prefixes[j]);
			if (strncmp(arg, context_prefixes[j], prefix_len) == 0) {
				if (!bounded_rg_context(arg + prefix_len)) {
					return 0;
				}
				matched_context = 1;
				break;
			}
		}
		if (matched_context) {
			continue;
		}
		if (strlen(arg) > 2 && (strncmp(arg, "-C", 2) == 0 || strncmp(arg, "-A", 2) == 0 || strncmp(arg, "-B", 2) == 0)) {
			if (!bounded_rg_context(arg + 2)) {
				return 0;
			}
			continue;
		}
		if (strlen(arg) > 2 && strncmp(arg, "-g", 2) == 0) {
			if (!bounded_rg_text(arg + 2, 1024)) {
				return 0;
			}
			continue;
		}
		if (arg[0] == '-') {
			return 0;
		}
		if (!seen_pattern) {
			if (!bounded_rg_text(arg, 2048)) {
				return 0;
			}
			seen_pattern = 1;
			continue;
		}
		if (strcmp(arg, ".") != 0 && !safe_relative_inspection_path_arg(arg)) {
			return 0;
		}
		path_count++;
	}
	return seen_pattern && path_count > 0;
}

static int append_normalized_epoch_input(byte_buf *b, policy_invocation *inv) {
	return bytes_append_argv_norm(b, inv);
}

static int repo_summary_epoch_uncached(policy_invocation *inv, char epoch[256]) {
	if (!is_git_head_subject_log(inv) && !is_git_ls_files(inv) && !is_git_status(inv) &&
	    !is_git_read_only_diff(inv) && !is_fixed_rg_repo_search(inv) && !is_bounded_rg_repo_search(inv)) {
		return 0;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF];
	if (!guarded_git_context(inv, repo_root, git_dir)) {
		return 0;
	}
	executable_signal tool;
	int rg_search = is_fixed_rg_repo_search(inv) || is_bounded_rg_repo_search(inv);
	if (!guarded_executable_signal(inv->cwd, rg_search ? "rg" : "git", &tool)) {
		return 0;
	}
	char index_fp[HASH_HEX + 16], config_fp[HASH_HEX], tree[HASH_HEX], content[HASH_HEX], input_hash[HASH_HEX];
	if (rg_search) {
		char ignore_fp[HASH_HEX], rg_config_fp[HASH_HEX], rg_env_fp[HASH_HEX];
		if (!exact_workspace_epochs(repo_root, 10000, 1, tree, content) ||
		    !guarded_git_ignore_fingerprint(repo_root, git_dir, ignore_fp) ||
		    !guarded_rg_config_fingerprint(rg_config_fp) ||
		    !ripgrep_environment_fingerprint(rg_env_fp)) {
			return 0;
		}
		byte_buf rg_input = {0};
		int rg_ok = bytes_append_str(&rg_input, repo_root) &&
		            bytes_append_byte(&rg_input, '|') &&
		            append_normalized_epoch_input(&rg_input, inv) &&
		            bytes_append_byte(&rg_input, '|') &&
		            bytes_append_str(&rg_input, ignore_fp) &&
		            bytes_append_byte(&rg_input, '|') &&
		            bytes_append_str(&rg_input, rg_config_fp) &&
		            bytes_append_byte(&rg_input, '|') &&
		            bytes_append_str(&rg_input, rg_env_fp) &&
		            bytes_append_byte(&rg_input, '|') &&
		            bytes_append_str(&rg_input, tree) &&
		            bytes_append_byte(&rg_input, '|') &&
		            bytes_append_str(&rg_input, content) &&
		            bytes_append_byte(&rg_input, '|') &&
		            bytes_append_str(&rg_input, tool.file_hash);
		if (rg_ok) {
			sha256_hex_buf(&rg_input, input_hash);
			snprintf(epoch, 256, "hot-repo-summary:%s:%s",
			         is_bounded_rg_repo_search(inv) ? "rg-bounded" : "rg-fixed", input_hash);
		}
		bytes_free(&rg_input);
		return rg_ok;
	}
	if (!guarded_git_index_fingerprint(git_dir, index_fp) ||
	    !guarded_git_config_fingerprint(repo_root, git_dir, config_fp)) {
		return 0;
	}
	byte_buf b = {0};
	int ok = 0;
	if (is_git_head_subject_log(inv)) {
		char head[128], branch[PATH_BUF], log_view_fp[HASH_HEX];
		if (!guarded_git_head(git_dir, head, branch) || !guarded_git_log_view_fingerprint(git_dir, log_view_fp)) {
			bytes_free(&b);
			return 0;
		}
		ok = bytes_append_str(&b, repo_root) &&
		     bytes_append_byte(&b, '|') &&
		     append_normalized_epoch_input(&b, inv) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, head) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, branch) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, config_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, log_view_fp) &&
		     bytes_append_byte(&b, '|') &&
		     bytes_append_str(&b, tool.file_hash);
		if (ok) {
			sha256_hex_buf(&b, input_hash);
			snprintf(epoch, 256, "hot-repo-summary:git-log-head-subject:%s", input_hash);
		}
		bytes_free(&b);
		return ok;
	}
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
		if (!guarded_git_head(git_dir, head, branch) || !guarded_git_ignore_fingerprint(repo_root, git_dir, ignore_fp)) {
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
		if (!guarded_git_attribute_fingerprint(repo_root, git_dir, attr_fp)) {
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

static int repo_search_corpus_epoch_uncached(policy_invocation *inv, char epoch[256]) {
	const char *rg_config_path = getenv("RIPGREP_CONFIG_PATH");
	if (inv == NULL || (rg_config_path != NULL && rg_config_path[0] != '\0')) {
		return 0;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF];
	if (!guarded_git_context(inv, repo_root, git_dir)) {
		return 0;
	}
	executable_signal tool;
	char tree[HASH_HEX], content[HASH_HEX], ignore_fp[HASH_HEX], config_fp[HASH_HEX], env_fp[HASH_HEX];
	if (!guarded_executable_signal(inv->cwd, "rg", &tool) ||
	    !exact_workspace_epochs(repo_root, 10000, 1, tree, content) ||
	    !guarded_git_ignore_fingerprint(repo_root, git_dir, ignore_fp) ||
	    !guarded_rg_config_fingerprint(config_fp) ||
	    !ripgrep_environment_fingerprint(env_fp)) {
		return 0;
	}
	byte_buf input = {0};
	int ok = bytes_append_str(&input, repo_root) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, ignore_fp) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, config_fp) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, env_fp) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, tree) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, content) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, tool.file_hash);
	if (ok) {
		char hash[HASH_HEX];
		sha256_hex_buf(&input, hash);
		snprintf(epoch, 256, "hot-repo-search-corpus:%s", hash);
	}
	bytes_free(&input);
	return ok;
}

static int git_history_corpus_epoch_uncached(policy_invocation *inv, char epoch[256]) {
	if (inv == NULL) {
		return 0;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF], head[128], branch[PATH_BUF];
	if (!guarded_git_context(inv, repo_root, git_dir) || !guarded_git_head(git_dir, head, branch)) {
		return 0;
	}
	executable_signal tool;
	char config_fp[HASH_HEX], log_view_fp[HASH_HEX], object_namespace_fp[HASH_HEX];
	if (!guarded_executable_signal(inv->cwd, "git", &tool) ||
	    !guarded_git_config_fingerprint(repo_root, git_dir, config_fp) ||
	    !guarded_git_log_view_fingerprint(git_dir, log_view_fp) ||
	    !guarded_git_object_namespace_fingerprint(git_dir, object_namespace_fp)) {
		return 0;
	}
	byte_buf input = {0};
	int ok = bytes_append_str(&input, repo_root) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, head) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, branch) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, config_fp) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, log_view_fp) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, object_namespace_fp) &&
	         bytes_append_byte(&input, '|') &&
	         bytes_append_str(&input, tool.file_hash);
	if (ok) {
		char hash[HASH_HEX];
		sha256_hex_buf(&input, hash);
		snprintf(epoch, 256, "hot-git-history-corpus:%s", hash);
	}
	bytes_free(&input);
	return ok;
}

#if defined(SQUIRE_MMAP_HOT_API) && (defined(__APPLE__) || defined(__linux__))
#define REPO_WORLD_CACHE_SLOTS 4
#define REPO_WORLD_COMMAND_SLOTS 256
#define REPO_WORLD_ENV_MAX_ENTRIES 4096
#define REPO_WORLD_ENV_MAX_BYTES (1024U * 1024U)

typedef struct {
	char command_key[HASH_HEX];
	char epoch[256];
	unsigned long long last_used;
	int occupied;
} repo_world_command_entry;

typedef struct {
	char root[PATH_BUF];
	char environment_hash[HASH_HEX];
	repo_world_command_entry commands[REPO_WORLD_COMMAND_SLOTS];
	unsigned long long last_used;
	int occupied;
	repo_change_guard guard;
} repo_world_cache_entry;

static repo_world_cache_entry repo_world_cache[REPO_WORLD_CACHE_SLOTS];
static pthread_mutex_t repo_world_cache_mu = PTHREAD_MUTEX_INITIALIZER;
static unsigned long long repo_world_cache_clock;
typedef int (*repo_epoch_compute_fn)(policy_invocation *inv, char epoch[256]);

static int repo_world_environment_hash(char out[HASH_HEX]) {
	size_t total = 0;
	size_t count = 0;
	if (environ != NULL) {
		for (; environ[count] != NULL; count++) {
			if (count >= REPO_WORLD_ENV_MAX_ENTRIES) {
				return 0;
			}
			size_t len = strlen(environ[count]);
			if (len > REPO_WORLD_ENV_MAX_BYTES - total) {
				return 0;
			}
			total += len;
		}
	}
	SQUIRE_SHA256_CTX ctx;
	SQUIRE_SHA256_Init(&ctx);
	for (size_t i = 0; i < count; i++) {
		SQUIRE_SHA256_Update(&ctx, environ[i], strlen(environ[i]));
		unsigned char zero = 0;
		SQUIRE_SHA256_Update(&ctx, &zero, 1);
	}
	SQUIRE_SHA256_Update(&ctx, &count, sizeof(count));
	unsigned char digest[32];
	static const char hex[] = "0123456789abcdef";
	SQUIRE_SHA256_Final(digest, &ctx);
	for (size_t i = 0; i < sizeof(digest); i++) {
		out[i * 2] = hex[digest[i] >> 4];
		out[i * 2 + 1] = hex[digest[i] & 0x0f];
	}
	out[64] = '\0';
	return 1;
}

static void repo_world_cache_clear(repo_world_cache_entry *entry) {
	if (entry == NULL) {
		return;
	}
	if (entry->guard.root[0] != '\0') {
		repo_guard_release(&entry->guard);
	}
	memset(entry, 0, sizeof(*entry));
	entry->guard.backend_fd = -1;
}

static repo_world_command_entry *repo_world_find_command(repo_world_cache_entry *entry, const char *key) {
	for (size_t i = 0; i < REPO_WORLD_COMMAND_SLOTS; i++) {
		if (entry->commands[i].occupied && strcmp(entry->commands[i].command_key, key) == 0) {
			return &entry->commands[i];
		}
	}
	return NULL;
}

static void repo_world_store_command(repo_world_cache_entry *entry, const char *key, const char *epoch) {
	repo_world_command_entry *selected = NULL;
	for (size_t i = 0; i < REPO_WORLD_COMMAND_SLOTS; i++) {
		repo_world_command_entry *candidate = &entry->commands[i];
		if (!candidate->occupied) {
			selected = candidate;
			break;
		}
		if (selected == NULL || candidate->last_used < selected->last_used) {
			selected = candidate;
		}
	}
	if (selected == NULL) {
		return;
	}
	memset(selected, 0, sizeof(*selected));
	selected->occupied = 1;
	snprintf(selected->command_key, sizeof(selected->command_key), "%s", key);
	snprintf(selected->epoch, sizeof(selected->epoch), "%s", epoch);
	selected->last_used = ++repo_world_cache_clock;
}

static int repo_world_epoch(policy_invocation *inv, char epoch[256], const char *domain,
	                        int command_scoped, repo_epoch_compute_fn compute) {
	char repo_root[PATH_BUF], git_dir[PATH_BUF];
	if (domain == NULL || domain[0] == '\0' || !discover_git_dir(inv->cwd, repo_root, git_dir)) {
		mmap_trace_path("repo-world-miss-discovery", domain);
		return 0;
	}
	char environment_hash[HASH_HEX];
	if (!repo_world_environment_hash(environment_hash)) {
		mmap_trace_path("repo-world-miss-environment", domain);
		return 0;
	}
	char invocation_key[HASH_HEX] = {0}, key_input[HASH_HEX + 64], key[HASH_HEX];
	if (command_scoped) {
		command_key(inv, invocation_key);
	}
	int key_len = snprintf(key_input, sizeof(key_input), "%s:%s", domain,
	                       command_scoped ? invocation_key : "workspace");
	if (key_len <= 0 || (size_t)key_len >= sizeof(key_input)) {
		mmap_trace_path("repo-world-miss-key", domain);
		return 0;
	}
	sha256_hex_str(key_input, key);
	pthread_mutex_lock(&repo_world_cache_mu);
	repo_world_cache_entry *world = NULL;
	for (size_t i = 0; i < REPO_WORLD_CACHE_SLOTS; i++) {
		repo_world_cache_entry *entry = &repo_world_cache[i];
		if (!entry->occupied || strcmp(entry->root, repo_root) != 0) {
			continue;
		}
		if (strcmp(entry->environment_hash, environment_hash) != 0) {
			mmap_trace_path("repo-world-reset-environment", domain);
			repo_world_cache_clear(entry);
			break;
		}
		if (!repo_guard_drain_clean(&entry->guard)) {
			mmap_trace_path("repo-world-reset-dirty", domain);
			repo_world_cache_clear(entry);
			break;
		}
		world = entry;
		break;
	}

	if (world != NULL) {
		world->last_used = ++repo_world_cache_clock;
		repo_world_command_entry *command = repo_world_find_command(world, key);
		if (command != NULL) {
			command->last_used = ++repo_world_cache_clock;
			snprintf(epoch, 256, "%s", command->epoch);
			pthread_mutex_unlock(&repo_world_cache_mu);
			return 1;
		}
		active_repo_guard = &world->guard;
		int ok = compute(inv, epoch);
		active_repo_guard = NULL;
		if (!ok) {
			mmap_trace_path("repo-world-miss-compute", domain);
			repo_world_cache_clear(world);
			pthread_mutex_unlock(&repo_world_cache_mu);
			return 0;
		}
		if (!repo_guard_drain_clean(&world->guard)) {
			mmap_trace_path("repo-world-miss-post-compute-dirty", domain);
			repo_world_cache_clear(world);
			pthread_mutex_unlock(&repo_world_cache_mu);
			return 0;
		}
		repo_world_store_command(world, key, epoch);
		pthread_mutex_unlock(&repo_world_cache_mu);
		return 1;
	}

	repo_world_cache_entry *selected = NULL;
	for (size_t i = 0; i < REPO_WORLD_CACHE_SLOTS; i++) {
		repo_world_cache_entry *entry = &repo_world_cache[i];
		if (!entry->occupied) {
			selected = entry;
			break;
		}
		if (selected == NULL || entry->last_used < selected->last_used) {
			selected = entry;
		}
	}
	if (selected == NULL) {
		pthread_mutex_unlock(&repo_world_cache_mu);
		return 0;
	}

	for (int attempt = 0; attempt < 2; attempt++) {
		repo_world_cache_clear(selected);
		if (!repo_guard_init(&selected->guard, repo_root)) {
			mmap_trace_path("repo-world-miss-guard-init", domain);
			break;
		}
		active_repo_guard = &selected->guard;
		int ok = compute(inv, epoch);
		active_repo_guard = NULL;
		if (!ok) {
			mmap_trace_path("repo-world-retry-compute", domain);
		}
		int clean = ok && repo_guard_drain_clean(&selected->guard);
		if (!clean) {
			if (ok) {
				mmap_trace_path("repo-world-retry-dirty", domain);
			}
			continue;
		}
		selected->occupied = 1;
		snprintf(selected->root, sizeof(selected->root), "%s", repo_root);
		snprintf(selected->environment_hash, sizeof(selected->environment_hash), "%s", environment_hash);
		selected->last_used = ++repo_world_cache_clock;
		repo_world_store_command(selected, key, epoch);
		if (getenv("SQUIRE_SHIM_DEBUG") != NULL) {
#if defined(__APPLE__)
			fprintf(stderr, "squire mmap proof debug: repo-world-build root=%s key=%s epoch=%s watches=%zu\n",
			        repo_root, key, epoch, selected->guard.watch_count);
#else
			fprintf(stderr, "squire mmap proof debug: repo-world-build root=%s key=%s epoch=%s\n", repo_root, key, epoch);
#endif
		}
		pthread_mutex_unlock(&repo_world_cache_mu);
		return 1;
	}
	repo_world_cache_clear(selected);
	mmap_trace_path("repo-world-miss-retries-exhausted", domain);
	pthread_mutex_unlock(&repo_world_cache_mu);
	return 0;
}

static int git_metadata_epoch(policy_invocation *inv, char epoch[256]) {
	if (!is_git_metadata(inv)) {
		return 0;
	}
	return repo_world_epoch(inv, epoch, "git-metadata", 1, git_metadata_epoch_uncached);
}

static int repo_summary_epoch(policy_invocation *inv, char epoch[256]) {
	if (!is_git_head_subject_log(inv) && !is_git_ls_files(inv) && !is_git_status(inv) &&
	    !is_git_read_only_diff(inv) && !is_fixed_rg_repo_search(inv) && !is_bounded_rg_repo_search(inv)) {
		return 0;
	}
	return repo_world_epoch(inv, epoch, "repo-summary", 1, repo_summary_epoch_uncached);
}

static int repo_search_corpus_epoch(policy_invocation *inv, char epoch[256]) {
	return repo_world_epoch(inv, epoch, "repo-search-corpus", 0, repo_search_corpus_epoch_uncached);
}

static int git_history_corpus_epoch(policy_invocation *inv, char epoch[256]) {
	return repo_world_epoch(inv, epoch, "git-history-corpus", 1, git_history_corpus_epoch_uncached);
}
#else
static int git_metadata_epoch(policy_invocation *inv, char epoch[256]) {
	return git_metadata_epoch_uncached(inv, epoch);
}

static int repo_summary_epoch(policy_invocation *inv, char epoch[256]) {
	return repo_summary_epoch_uncached(inv, epoch);
}

static int repo_search_corpus_epoch(policy_invocation *inv, char epoch[256]) {
	return repo_search_corpus_epoch_uncached(inv, epoch);
}

static int git_history_corpus_epoch(policy_invocation *inv, char epoch[256]) {
	return git_history_corpus_epoch_uncached(inv, epoch);
}
#endif

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
		".jsonc", ".toml", ".yaml", ".yml", ".xml", ".md", ".markdown", ".rst", ".txt", ".log",
	};
	for (size_t i = 0; i < sizeof(exts) / sizeof(exts[0]); i++) {
		if (has_ext(name, exts[i])) {
			return 1;
		}
	}
	return 0;
}

#define MAX_LINE_SELECTION_RANGES 8

typedef struct {
	int start;
	int end;
} line_range;

typedef struct {
	line_range ranges[MAX_LINE_SELECTION_RANGES];
	int count;
} line_selection;

static int parse_line_selection_number(const char **cursor, int *value) {
	const char *p = *cursor;
	if (p == NULL || !isdigit((unsigned char)*p)) {
		return 0;
	}
	int parsed = 0;
	while (isdigit((unsigned char)*p)) {
		parsed = parsed * 10 + (*p - '0');
		if (parsed > 10000) {
			return 0;
		}
		p++;
	}
	if (parsed <= 0) {
		return 0;
	}
	*cursor = p;
	*value = parsed;
	return 1;
}

static int parse_sed_print_selection(const char *expr, line_selection *selection) {
	if (expr == NULL || selection == NULL || expr[0] == '\0') {
		return 0;
	}
	memset(selection, 0, sizeof(*selection));
	const char *p = expr;
	while (*p != '\0') {
		if (selection->count >= MAX_LINE_SELECTION_RANGES) {
			return 0;
		}
		int start = 0;
		int end = 0;
		if (!parse_line_selection_number(&p, &start)) {
			return 0;
		}
		end = start;
		if (*p == ',') {
			p++;
			if (!parse_line_selection_number(&p, &end)) {
				return 0;
			}
		}
		if (*p != 'p' || end < start || end - start > 500) {
			return 0;
		}
		selection->ranges[selection->count].start = start;
		selection->ranges[selection->count].end = end;
		selection->count++;
		p++;
		if (*p == '\0') {
			return 1;
		}
		if (*p != ';' || p[1] == '\0') {
			return 0;
		}
		p++;
	}
	return 0;
}

static int line_selection_max_end(const line_selection *selection) {
	int max = 0;
	if (selection == NULL) {
		return max;
	}
	for (int i = 0; i < selection->count; i++) {
		if (selection->ranges[i].end > max) {
			max = selection->ranges[i].end;
		}
	}
	return max;
}

static int line_selection_match_count(const line_selection *selection, int line) {
	int matches = 0;
	if (selection == NULL) {
		return matches;
	}
	for (int i = 0; i < selection->count; i++) {
		if (line >= selection->ranges[i].start && line <= selection->ranges[i].end) {
			matches++;
		}
	}
	return matches;
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

static int parse_nl_all_args(policy_invocation *inv, const char **path) {
	if (inv == NULL || inv->argc != 3 || strcmp(inv->argv[0], "nl") != 0 || strcmp(inv->argv[1], "-ba") != 0) {
		return 0;
	}
	*path = inv->argv[2];
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
#ifdef SQUIRE_MMAP_HOT_API
	return !env_truthy("SQUIRE_HOT_DISABLE_WARM_FILE_REPLAY");
#else
	return env_truthy("SQUIRE_SHIM_ENABLE_WARM_FILE_REPLAY") || env_truthy("SQUIRE_SHIM_REQUIRE_HIT");
#endif
}

typedef enum {
	WARM_FILE_OPERATION_NONE = 0,
	WARM_FILE_OPERATION_CAT,
	WARM_FILE_OPERATION_SED,
	WARM_FILE_OPERATION_HEAD,
	WARM_FILE_OPERATION_TAIL,
	WARM_FILE_OPERATION_NL,
	WARM_FILE_OPERATION_GREP,
	WARM_FILE_OPERATION_RG,
} warm_file_operation_kind;

typedef struct {
	warm_file_operation_kind kind;
	const char *path;
	line_selection selection;
	int line_count;
	const char *pattern;
	int quiet;
	int line_number;
} warm_file_operation;

static int parse_warm_file_operation(policy_invocation *inv, warm_file_operation *operation) {
	if (inv == NULL || operation == NULL || inv->argc <= 0) {
		return 0;
	}
	memset(operation, 0, sizeof(*operation));
	if (inv->argc == 2 && strcmp(inv->argv[0], "cat") == 0) {
		operation->kind = WARM_FILE_OPERATION_CAT;
		operation->path = inv->argv[1];
		return 1;
	}
	if (inv->argc == 4 && strcmp(inv->argv[0], "sed") == 0 && strcmp(inv->argv[1], "-n") == 0) {
		if (!parse_sed_print_selection(inv->argv[2], &operation->selection)) {
			return 0;
		}
		operation->kind = WARM_FILE_OPERATION_SED;
		operation->path = inv->argv[3];
		return 1;
	}
	if (strcmp(inv->argv[0], "head") == 0 || strcmp(inv->argv[0], "tail") == 0) {
		int tail = strcmp(inv->argv[0], "tail") == 0;
		if (!parse_head_tail_args(inv, tail, &operation->path, &operation->line_count)) {
			return 0;
		}
		operation->kind = tail ? WARM_FILE_OPERATION_TAIL : WARM_FILE_OPERATION_HEAD;
		return 1;
	}
	if (strcmp(inv->argv[0], "nl") == 0) {
		if (!parse_nl_all_args(inv, &operation->path)) {
			return 0;
		}
		operation->kind = WARM_FILE_OPERATION_NL;
		return 1;
	}
	if (strcmp(inv->argv[0], "grep") == 0) {
		if (!parse_fixed_grep_args(inv, &operation->pattern, &operation->path, &operation->quiet)) {
			return 0;
		}
		operation->kind = WARM_FILE_OPERATION_GREP;
		return 1;
	}
	if (strcmp(inv->argv[0], "rg") == 0) {
		if (!parse_fixed_rg_args(inv, &operation->pattern, &operation->path, &operation->quiet, &operation->line_number)) {
			return 0;
		}
		operation->kind = WARM_FILE_OPERATION_RG;
		return 1;
	}
	return 0;
}

static int is_warm_file_candidate(policy_invocation *inv) {
	warm_file_operation operation;
	return parse_warm_file_operation(inv, &operation);
}

static int warm_file_proof(policy_invocation *inv, char key[HASH_HEX], char epoch[256], char path[PATH_BUF], warm_file_operation *operation_out,
                           char content_hash_out[HASH_HEX], unsigned char **content_out, size_t *content_len_out) {
	warm_file_operation operation;
	if (!parse_warm_file_operation(inv, &operation)) {
		return 0;
	}
	const char *tool_name = base_name(inv->argv[0]);
	executable_signal tool;
	if (tool_name == NULL || !executable_signal_for(inv->cwd, tool_name, &tool)) {
		return 0;
	}
	if (content_out != NULL) {
		*content_out = NULL;
	}
	if (content_len_out != NULL) {
		*content_len_out = 0;
	}
	char rel[PATH_BUF];
	if (!clean_relative_path(operation.path, rel)) {
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
	char content_hash[HASH_HEX];
	unsigned char *proof_content = NULL;
	size_t proof_content_len = 0;
	struct stat st;
	if (!hot_file_read_proven(path, &st, content_hash, content_out != NULL ? &proof_content : NULL,
	                          content_out != NULL ? &proof_content_len : NULL)) {
		return 0;
	}
	if (operation.kind == WARM_FILE_OPERATION_CAT && st.st_size > MAX_FILE_OUTPUT_BYTES) {
		free(proof_content);
		return 0;
	}
	if (content_hash_out != NULL) {
		memcpy(content_hash_out, content_hash, HASH_HEX);
	}
	char mode[32];
	if (!mode_string(st.st_mode, mode)) {
		free(proof_content);
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
	if (content_out != NULL) {
		*content_out = proof_content;
		if (content_len_out != NULL) {
			*content_len_out = proof_content_len;
		}
	}
	if (operation_out != NULL) {
		*operation_out = operation;
	}
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
	snap->cache_token = NULL;
	return 1;
}

static pthread_once_t inherited_snapshot_once = PTHREAD_ONCE_INIT;
static int inherited_snapshot_fd = -1;
static unsigned char *inherited_snapshot_data;
static size_t inherited_snapshot_len;

static void initialize_inherited_snapshot(void) {
	int fd = hot_snapshot_fd();
	struct stat st;
	if (fd < 0 || fstat(fd, &st) != 0 || st.st_size < HOT_HEADER_BYTES || st.st_size > HOT_MAX_BYTES) {
		return;
	}
	unsigned char *data = mmap(NULL, (size_t)st.st_size, PROT_READ, MAP_SHARED, fd, 0);
	if (data == MAP_FAILED) {
		return;
	}
	inherited_snapshot_fd = fd;
	inherited_snapshot_data = data;
	inherited_snapshot_len = (size_t)st.st_size;
}

static int map_snapshot_fd_cached(int fd, mapped_snapshot *snap) {
	pthread_once(&inherited_snapshot_once, initialize_inherited_snapshot);
	if (inherited_snapshot_fd == fd && inherited_snapshot_data != NULL && inherited_snapshot_len >= HOT_HEADER_BYTES) {
		snap->data = inherited_snapshot_data;
		snap->len = inherited_snapshot_len;
		snap->borrowed = 1;
		snap->cache_token = NULL;
		return 1;
	}
	return map_snapshot_fd(fd, snap);
}

#define HOT_SNAPSHOT_CACHE_SLOTS 4

typedef struct {
	char path[PATH_BUF];
	unsigned char *data;
	size_t len;
	dev_t dev;
	ino_t ino;
	long long mtime_ns;
	long long ctime_ns;
	unsigned int references;
	unsigned long long last_used;
} hot_snapshot_cache_entry;

static hot_snapshot_cache_entry hot_snapshot_cache[HOT_SNAPSHOT_CACHE_SLOTS];
static pthread_rwlock_t hot_snapshot_cache_lock = PTHREAD_RWLOCK_INITIALIZER;
static unsigned long long hot_snapshot_cache_clock;

static int same_snapshot_identity(const hot_snapshot_cache_entry *entry, const char *path, const struct stat *st) {
	return entry->data != NULL && strcmp(entry->path, path) == 0 &&
	       entry->dev == st->st_dev && entry->ino == st->st_ino &&
	       entry->len == (size_t)st->st_size && entry->mtime_ns == stat_mtime_nano(st) &&
	       entry->ctime_ns == stat_ctime_nano(st);
}

static int map_snapshot_path_cached(const char *path, mapped_snapshot *snap) {
	struct stat path_st;
	if (stat(path, &path_st) != 0 || !S_ISREG(path_st.st_mode) ||
	    path_st.st_size < HOT_HEADER_BYTES || path_st.st_size > HOT_MAX_BYTES) {
		return 0;
	}
	pthread_rwlock_rdlock(&hot_snapshot_cache_lock);
	for (size_t i = 0; i < HOT_SNAPSHOT_CACHE_SLOTS; i++) {
		hot_snapshot_cache_entry *entry = &hot_snapshot_cache[i];
		if (!same_snapshot_identity(entry, path, &path_st)) {
			continue;
		}
		__atomic_add_fetch(&entry->references, 1U, __ATOMIC_ACQ_REL);
		snap->data = entry->data;
		snap->len = entry->len;
		snap->borrowed = 1;
		snap->cache_token = entry;
		pthread_rwlock_unlock(&hot_snapshot_cache_lock);
		mmap_trace_path("snapshot-cache-hit", path);
		return 1;
	}
	pthread_rwlock_unlock(&hot_snapshot_cache_lock);
	mmap_trace_path("snapshot-cache-refresh", path);

	int fd = open(path, O_RDONLY);
	if (fd < 0) {
		return 0;
	}
	struct stat st;
	if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size < HOT_HEADER_BYTES || st.st_size > HOT_MAX_BYTES) {
		close(fd);
		return 0;
	}
	unsigned char *data = mmap(NULL, (size_t)st.st_size, PROT_READ, MAP_SHARED, fd, 0);
	close(fd);
	if (data == MAP_FAILED) {
		return 0;
	}

	pthread_rwlock_wrlock(&hot_snapshot_cache_lock);
	for (size_t i = 0; i < HOT_SNAPSHOT_CACHE_SLOTS; i++) {
		hot_snapshot_cache_entry *entry = &hot_snapshot_cache[i];
		if (!same_snapshot_identity(entry, path, &st)) {
			continue;
		}
		munmap(data, (size_t)st.st_size);
		__atomic_add_fetch(&entry->references, 1U, __ATOMIC_ACQ_REL);
		entry->last_used = ++hot_snapshot_cache_clock;
		snap->data = entry->data;
		snap->len = entry->len;
		snap->borrowed = 1;
		snap->cache_token = entry;
		pthread_rwlock_unlock(&hot_snapshot_cache_lock);
		mmap_trace_path("snapshot-cache-raced-hit", path);
		return 1;
	}
	hot_snapshot_cache_entry *selected = NULL;
	for (size_t i = 0; i < HOT_SNAPSHOT_CACHE_SLOTS; i++) {
		hot_snapshot_cache_entry *entry = &hot_snapshot_cache[i];
		if (__atomic_load_n(&entry->references, __ATOMIC_ACQUIRE) != 0) {
			continue;
		}
		if (selected == NULL || entry->data == NULL || entry->last_used < selected->last_used) {
			selected = entry;
			if (entry->data == NULL) {
				break;
			}
		}
	}
	if (selected == NULL) {
		pthread_rwlock_unlock(&hot_snapshot_cache_lock);
		snap->data = data;
		snap->len = (size_t)st.st_size;
		snap->borrowed = 0;
		snap->cache_token = NULL;
		return 1;
	}
	if (selected->data != NULL) {
		munmap(selected->data, selected->len);
	}
	memset(selected, 0, sizeof(*selected));
	snprintf(selected->path, sizeof(selected->path), "%s", path);
	selected->data = data;
	selected->len = (size_t)st.st_size;
	selected->dev = st.st_dev;
	selected->ino = st.st_ino;
	selected->mtime_ns = stat_mtime_nano(&st);
	selected->ctime_ns = stat_ctime_nano(&st);
	__atomic_store_n(&selected->references, 1U, __ATOMIC_RELEASE);
	selected->last_used = ++hot_snapshot_cache_clock;
	snap->data = selected->data;
	snap->len = selected->len;
	snap->borrowed = 1;
	snap->cache_token = selected;
	pthread_rwlock_unlock(&hot_snapshot_cache_lock);
	mmap_trace_path("snapshot-cache-store", path);
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
	return map_snapshot_path_cached(snapshot_path, snap);
}

static void unmap_snapshot(mapped_snapshot *snap) {
	if (snap->cache_token != NULL) {
		hot_snapshot_cache_entry *entry = (hot_snapshot_cache_entry *)snap->cache_token;
		unsigned int previous = __atomic_fetch_sub(&entry->references, 1U, __ATOMIC_ACQ_REL);
		if (previous == 0) {
			__atomic_store_n(&entry->references, 0U, __ATOMIC_RELEASE);
		}
	} else if (snap->data != NULL && !snap->borrowed) {
		munmap(snap->data, snap->len);
	}
	snap->data = NULL;
	snap->len = 0;
	snap->borrowed = 0;
	snap->cache_token = NULL;
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
		mmap_trace_path("snapshot-miss-command", command_hash);
		return 0;
	}
	int saw_command = 0;
	for (uint32_t i = start; i < count; i++) {
		unsigned char *entry = snap->data + HOT_HEADER_BYTES + i * HOT_ENTRY_BYTES;
		int key_cmp = snapshot_key_compare(entry, command_hash);
		if (key_cmp != 0) {
			break;
		}
		saw_command = 1;
		if (memcmp(entry + 64, epoch_hash, 64) != 0) {
			if (i == start && mmap_trace_enabled()) {
				char available_epoch_hash[HASH_HEX];
				memcpy(available_epoch_hash, entry + 64, 64);
				available_epoch_hash[64] = '\0';
				mmap_trace_path("snapshot-miss-want-epoch-hash", epoch_hash);
				mmap_trace_path("snapshot-miss-first-epoch-hash", available_epoch_hash);
			}
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
	if (saw_command) {
		mmap_trace_path("snapshot-miss-epoch", command_hash);
	}
	return 0;
}

static int output_line_selection(const unsigned char *content, uint32_t len, const line_selection *selection, size_t max_output) {
	if (selection == NULL || selection->count <= 0) {
		return 0;
	}
	size_t output_len = 0;
	int line = 1;
	int max_end = line_selection_max_end(selection);
	uint32_t offset = 0;
	while (offset < len && line <= max_end) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		if (line_end < len && content[line_end] == '\n') {
			line_end++;
		}
		int matches = line_selection_match_count(selection, line);
		for (int match = 0; match < matches; match++) {
			size_t line_len = line_end - offset;
			if ((max_output > 0 && (output_len > max_output || line_len > max_output - output_len)) ||
			    !write_all(STDOUT_FILENO, content + offset, line_len)) {
				return 0;
			}
			output_len += line_len;
		}
		offset = line_end;
		line++;
	}
	return 1;
}

static int output_sed_range(const unsigned char *content, uint32_t len, int start, int end) {
	line_selection selection = {0};
	selection.ranges[0].start = start;
	selection.ranges[0].end = end;
	selection.count = 1;
	return output_line_selection(content, len, &selection, MAX_FILE_OUTPUT_BYTES);
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

static int is_default_nl_delimiter(const unsigned char *line, size_t len) {
	if (len != 2 && len != 4 && len != 6) {
		return 0;
	}
	for (size_t i = 0; i < len; i += 2) {
		if (line[i] != '\\' || line[i + 1] != ':') {
			return 0;
		}
	}
	return 1;
}

static int output_nl_all(const unsigned char *content, uint32_t len) {
	size_t output_len = 0;
	int line = 1;
	uint32_t offset = 0;
	while (offset < len) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		if (is_default_nl_delimiter(content + offset, line_end - offset)) {
			return 0;
		}
		char prefix[32];
		int prefix_len = snprintf(prefix, sizeof(prefix), "%6d\t", line);
		uint32_t write_end = line_end < len ? line_end + 1 : line_end;
		size_t line_len = write_end - offset;
		if (prefix_len <= 0 || prefix_len >= (int)sizeof(prefix) ||
		    output_len > MAX_FILE_OUTPUT_BYTES ||
		    (size_t)prefix_len > MAX_FILE_OUTPUT_BYTES - output_len ||
		    line_len > MAX_FILE_OUTPUT_BYTES - output_len - (size_t)prefix_len) {
			return 0;
		}
		output_len += (size_t)prefix_len + line_len;
		offset = write_end;
		line++;
	}
	line = 1;
	offset = 0;
	while (offset < len) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		uint32_t write_end = line_end < len ? line_end + 1 : line_end;
		char prefix[32];
		int prefix_len = snprintf(prefix, sizeof(prefix), "%6d\t", line);
		if (prefix_len <= 0 || prefix_len >= (int)sizeof(prefix) ||
		    !write_all(STDOUT_FILENO, prefix, (size_t)prefix_len) ||
		    !write_all(STDOUT_FILENO, content + offset, write_end - offset)) {
			return 0;
		}
		offset = write_end;
		line++;
	}
	return 1;
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
	const char *env = getenv("SQUIRE_STORE_ROOT");
	if (env != NULL && env[0] != '\0') {
		snprintf(store_root, PATH_BUF, "%s", env);
		return 1;
	}
	/* Compatibility with pre-1.0 Squire sessions. */
	env = getenv("SQUIRE_KERNEL_STORE_ROOT");
	if (env != NULL && env[0] != '\0') {
		snprintf(store_root, PATH_BUF, "%s", env);
		return 1;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF];
	if (!discover_git_dir(cwd, repo_root, git_dir)) {
		return 0;
	}
	return join_path(store_root, PATH_BUF, git_dir, "squire/state");
}

static int bytes_append_le32(byte_buf *b, uint32_t value) {
	unsigned char raw[4] = {
		(unsigned char)(value & 0xff),
		(unsigned char)((value >> 8) & 0xff),
		(unsigned char)((value >> 16) & 0xff),
		(unsigned char)((value >> 24) & 0xff),
	};
	return bytes_append(b, raw, sizeof(raw));
}

static int is_prepare_request_candidate(policy_invocation *inv) {
	char target[PATH_BUF], flag[16];
	const char *lookup_target = NULL;
	return is_git_head_subject_log(inv) || is_git_ls_files(inv) || is_git_status(inv) ||
	       is_git_read_only_diff(inv) || is_fixed_rg_repo_search(inv) || is_bounded_rg_repo_search(inv) ||
	       is_tool_version_probe(inv) || command_path_lookup_target(inv, &lookup_target) ||
	       is_static_environment_probe(inv) || is_printenv_probe(inv) ||
	       parse_directory_listing(inv, target, flag) || is_file_type_candidate(inv);
}

/*
 * A miss never executes through this path. It only publishes a bounded request
 * for the resident Go maintainer, which independently reparses the request and
 * applies the same read-only policy before preparing an exact snapshot.
 */
static int enqueue_prepare_request_at_cwd(const char *cwd, int argc, char **argv) {
	policy_invocation inv;
	if (!normalize_invocation_at_cwd(cwd, argc, argv, &inv) || !is_prepare_request_candidate(&inv)) {
		return 0;
	}
	char store_root[PATH_BUF];
	if (!discover_store_root(inv.cwd, store_root)) {
		return 0;
	}
	char request_dir[PATH_BUF];
	if (!join_path(request_dir, sizeof(request_dir), store_root, "prepare_requests") || !mkdir_p(request_dir)) {
		return 0;
	}
	char key[HASH_HEX];
	command_key(&inv, key);
	char filename[96];
	if (snprintf(filename, sizeof(filename), "%s.req", key) <= 0) {
		return 0;
	}
	char final_path[PATH_BUF];
	if (!join_path(final_path, sizeof(final_path), request_dir, filename)) {
		return 0;
	}
	struct stat existing;
	if (lstat(final_path, &existing) == 0) {
		return S_ISREG(existing.st_mode);
	}

	static const unsigned char magic[8] = {'S', 'Q', 'R', 'Q', '0', '0', '0', '1'};
	byte_buf body = {0};
	size_t cwd_len = strlen(inv.cwd);
	int ok = cwd_len > 0 && cwd_len < PATH_BUF &&
	         bytes_append(&body, magic, sizeof(magic)) &&
	         bytes_append_le32(&body, (uint32_t)cwd_len) &&
	         bytes_append_le32(&body, (uint32_t)inv.argc) &&
	         bytes_append(&body, inv.cwd, cwd_len);
	for (int i = 0; ok && i < inv.argc; i++) {
		size_t arg_len = strlen(inv.argv[i]);
		ok = arg_len > 0 && arg_len < PATH_BUF &&
		     bytes_append_le32(&body, (uint32_t)arg_len) &&
		     bytes_append(&body, inv.argv[i], arg_len) &&
		     body.len <= MAX_PREPARE_REQUEST_BYTES;
	}
	if (!ok || body.len > MAX_PREPARE_REQUEST_BYTES) {
		bytes_free(&body);
		return 0;
	}

	char temp_name[128];
	long long nonce = now_realtime_ns();
	if (snprintf(temp_name, sizeof(temp_name), ".%s.%ld.%lld.tmp", key, (long)getpid(), nonce) <= 0) {
		bytes_free(&body);
		return 0;
	}
	char temp_path[PATH_BUF];
	if (!join_path(temp_path, sizeof(temp_path), request_dir, temp_name)) {
		bytes_free(&body);
		return 0;
	}
	int fd = open(temp_path, O_WRONLY | O_CREAT | O_EXCL, 0600);
	if (fd < 0) {
		bytes_free(&body);
		return errno == EEXIST;
	}
	ok = write_all(fd, body.data, body.len);
	if (close(fd) != 0) {
		ok = 0;
	}
	bytes_free(&body);
	if (!ok || rename(temp_path, final_path) != 0) {
		(void)unlink(temp_path);
		return 0;
	}
	return 1;
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
		mmap_trace_path("exact-miss-key", key);
		mmap_trace_path("exact-miss-epoch", epoch);
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
	warm_file_operation operation;
	if (!warm_file_proof(inv, key, epoch, path, &operation, NULL, NULL, NULL)) {
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
	if (operation.kind == WARM_FILE_OPERATION_CAT) {
		if (content_len > MAX_FILE_OUTPUT_BYTES) {
			return 0;
		}
		if (content_len > 0 && !write_all(STDOUT_FILENO, content, content_len)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (operation.kind == WARM_FILE_OPERATION_SED) {
		if (!output_line_selection(content, content_len, &operation.selection, MAX_FILE_OUTPUT_BYTES)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (operation.kind == WARM_FILE_OPERATION_HEAD) {
		if (!output_sed_range(content, content_len, 1, operation.line_count)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (operation.kind == WARM_FILE_OPERATION_TAIL) {
		if (!output_tail_lines(content, content_len, operation.line_count)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (operation.kind == WARM_FILE_OPERATION_NL) {
		if (!output_nl_all(content, content_len)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(0);
	}
	if (operation.kind == WARM_FILE_OPERATION_GREP) {
		int matched = 0;
		if (!output_fixed_grep(content, content_len, operation.pattern, operation.quiet, &matched)) {
			return 0;
		}
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
		_exit(matched ? 0 : 1);
	}
	if (operation.kind == WARM_FILE_OPERATION_RG) {
		int matched = 0;
		if (!output_fixed_rg(content, content_len, operation.pattern, operation.quiet, operation.line_number, &matched)) {
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
		mmap_trace_path("exact-prepare-miss-key", key);
		mmap_trace_path("exact-prepare-miss-epoch", epoch);
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
	if (is_git_head_subject_log(&inv) || is_git_ls_files(&inv) || is_git_status(&inv) ||
	    is_git_read_only_diff(&inv) || is_fixed_rg_repo_search(&inv) || is_bounded_rg_repo_search(&inv)) {
		if (!repo_summary_epoch(&inv, epoch)) {
			mmap_trace_path("exact-prepare-miss-repo-summary-proof", inv.cwd);
		} else if (prepare_exact_replay_for_epoch(&prepared->snap, &inv, epoch, prepared)) {
			prepared->synthetic_safe = 1;
			return 1;
		}
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
