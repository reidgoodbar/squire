#define SQUIRE_MMAP_EMBEDDED
#define SQUIRE_MMAP_HOT_API 1
#define SQUIRE_MMAP_NO_MAIN 1
#define SQUIRE_PRELOAD_HELPER_NO_MAIN

#include "squire_hot_api.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "squire_preload_helper.c"

extern char **environ;

typedef enum {
	SQUIRE_HOT_HANDLE_EXACT = 1,
	SQUIRE_HOT_HANDLE_SCRIPT = 2,
} squire_hot_handle_kind;

typedef struct {
	squire_hot_handle_kind kind;
	prepared_exact_replay exact;
	helper_result script;
} squire_hot_handle;

static void squire_hot_trace(const char *message) {
	const char *enabled = getenv("SQUIRE_CODEX_BRIDGE_TRACE");
	if (enabled != NULL && (strcmp(enabled, "1") == 0 || strcmp(enabled, "true") == 0 || strcmp(enabled, "yes") == 0)) {
		fprintf(stderr, "squire hot api: %s\n", message);
		const char *path = getenv("SQUIRE_CODEX_BRIDGE_TRACE_FILE");
		if (path == NULL || path[0] == '\0') {
			path = "/tmp/squire-codex-bridge-trace.log";
		}
		FILE *f = fopen(path, "a");
		if (f != NULL) {
			fprintf(f, "squire hot api: %s\n", message);
			fclose(f);
		}
	}
}

static void squire_hot_fill_exact(squire_hot_result *out, squire_hot_handle *handle) {
	out->handle = handle;
	out->stdout_data = handle->exact.stdout_data;
	out->stdout_len = handle->exact.stdout_len;
	out->stderr_data = handle->exact.stderr_data;
	out->stderr_len = handle->exact.stderr_len;
	out->exit_code = handle->exact.exit_code;
	out->native_wall_ms = handle->exact.native_wall_ms;
}

static void squire_hot_fill_script(squire_hot_result *out, squire_hot_handle *handle) {
	out->handle = handle;
	out->stdout_data = handle->script.stdout_buf.data;
	out->stdout_len = handle->script.stdout_buf.len;
	out->stderr_data = handle->script.stderr_buf.data;
	out->stderr_len = handle->script.stderr_buf.len;
	out->exit_code = handle->script.exit_code;
	out->native_wall_ms = 0;
}

int squire_hot_try_replay_argv(const char *cwd, int argc, const char *const *argv, squire_hot_result *out) {
	if (out == NULL || argc <= 0 || argc > MAX_ARGC || argv == NULL) {
		return 0;
	}
	memset(out, 0, sizeof(*out));
	char *mutable_argv[MAX_ARGC];
	for (int i = 0; i < argc; i++) {
		if (argv[i] == NULL) {
			return 0;
		}
		mutable_argv[i] = (char *)argv[i];
	}
	squire_hot_handle *handle = (squire_hot_handle *)calloc(1, sizeof(*handle));
	if (handle == NULL) {
		return 0;
	}
	handle->kind = SQUIRE_HOT_HANDLE_EXACT;
	if (!prepare_exact_replay_at_cwd(cwd, argc, mutable_argv, &handle->exact)) {
		handle->kind = SQUIRE_HOT_HANDLE_SCRIPT;
		long long replay_start_ns = now_monotonic_ns();
		if (!helper_prepare_git_history_at_cwd(cwd, argc, mutable_argv, &handle->script, replay_start_ns) &&
		    !helper_prepare_repo_search_at_cwd(cwd, argc, mutable_argv, &handle->script, replay_start_ns, 0, 0) &&
		    !helper_prepare_warm_file_at_cwd(cwd, argc, mutable_argv, &handle->script, replay_start_ns)) {
			free(handle);
			return 0;
		}
		if (handle->script.stdout_buf.len > MAX_FAST_OUTPUT_BYTES || handle->script.stderr_buf.len > MAX_FAST_OUTPUT_BYTES) {
			helper_result_free(&handle->script);
			free(handle);
			return 0;
		}
		squire_hot_fill_script(out, handle);
		return 1;
	}
	squire_hot_fill_exact(out, handle);
	return 1;
}

static int squire_hot_try_replay_plan(const char *cwd, helper_plan *plan, squire_hot_result *out) {
	if (out == NULL || plan == NULL) {
		return 0;
	}
	memset(out, 0, sizeof(*out));
	squire_hot_handle *handle = (squire_hot_handle *)calloc(1, sizeof(*handle));
	if (handle == NULL) {
		return 0;
	}
	handle->kind = SQUIRE_HOT_HANDLE_SCRIPT;
	if (!helper_eval_shell_plan_at_cwd(cwd, plan, &handle->script, now_monotonic_ns())) {
		free(handle);
		return 0;
	}
	squire_hot_fill_script(out, handle);
	return 1;
}

int squire_hot_try_replay_script(const char *cwd, const char *script, squire_hot_result *out) {
	if (out == NULL || script == NULL || script[0] == '\0') {
		return 0;
	}
	helper_plan *plan = (helper_plan *)calloc(1, sizeof(*plan));
	if (plan == NULL) {
		return 0;
	}
	int replayed = helper_parse_shell_plan(script, plan) && squire_hot_try_replay_plan(cwd, plan, out);
	free(plan);
	return replayed;
}

static int squire_hot_key_eq(const char *entry, size_t key_len, const char *key) {
	return strlen(key) == key_len && strncmp(entry, key, key_len) == 0;
}

static int squire_hot_key_has_prefix(const char *entry, size_t key_len, const char *prefix) {
	size_t prefix_len = strlen(prefix);
	return key_len >= prefix_len && strncmp(entry, prefix, prefix_len) == 0;
}

/*
 * Compare only environment inputs that can change an accelerated command's
 * observable behavior. Codex keeps internal process metadata (for example,
 * CODEX_PERMISSION_PROFILE) out of child command environments; requiring
 * whole-process equality for those unrelated keys creates false misses.
 *
 * This is deliberately a conservative union across every supported command
 * family. Individual snapshot proofs still validate the narrower command-
 * specific fingerprints before replaying bytes.
 */
static int squire_hot_replay_env_key(const char *entry, size_t key_len) {
	return squire_hot_key_eq(entry, key_len, "PATH") ||
	       squire_hot_key_eq(entry, key_len, "HOME") ||
	       squire_hot_key_eq(entry, key_len, "USER") ||
	       squire_hot_key_eq(entry, key_len, "LOGNAME") ||
	       squire_hot_key_eq(entry, key_len, "SHELL") ||
	       squire_hot_key_eq(entry, key_len, "HOSTNAME") ||
	       squire_hot_key_eq(entry, key_len, "XDG_CONFIG_HOME") ||
	       squire_hot_key_eq(entry, key_len, "LANG") ||
	       squire_hot_key_has_prefix(entry, key_len, "LC_") ||
	       squire_hot_key_eq(entry, key_len, "TZ") ||
	       squire_hot_key_eq(entry, key_len, "TERM") ||
	       squire_hot_key_eq(entry, key_len, "COLORTERM") ||
	       squire_hot_key_eq(entry, key_len, "COLUMNS") ||
	       squire_hot_key_eq(entry, key_len, "PAGER") ||
	       squire_hot_key_eq(entry, key_len, "RIPGREP_CONFIG_PATH") ||
	       squire_hot_key_eq(entry, key_len, "NO_COLOR") ||
	       squire_hot_key_eq(entry, key_len, "CLICOLOR") ||
	       squire_hot_key_eq(entry, key_len, "CLICOLOR_FORCE") ||
	       squire_hot_key_eq(entry, key_len, "LSCOLORS") ||
	       squire_hot_key_eq(entry, key_len, "LS_COLORS") ||
	       squire_hot_key_eq(entry, key_len, "BLOCKSIZE") ||
	       squire_hot_key_eq(entry, key_len, "BLOCK_SIZE") ||
	       squire_hot_key_eq(entry, key_len, "LS_BLOCK_SIZE") ||
	       squire_hot_key_eq(entry, key_len, "TIME_STYLE") ||
	       squire_hot_key_eq(entry, key_len, "QUOTING_STYLE") ||
	       squire_hot_key_eq(entry, key_len, "TABSIZE") ||
	       squire_hot_key_eq(entry, key_len, "MAGIC") ||
	       squire_hot_key_eq(entry, key_len, "GREP_COLORS") ||
	       squire_hot_key_eq(entry, key_len, "GREP_OPTIONS") ||
	       squire_hot_key_eq(entry, key_len, "POSIXLY_CORRECT") ||
	       squire_hot_key_eq(entry, key_len, "_POSIX2_VERSION") ||
	       squire_hot_key_eq(entry, key_len, "COMMAND_MODE") ||
	       squire_hot_key_eq(entry, key_len, "GOROOT") ||
	       squire_hot_key_eq(entry, key_len, "GOTOOLCHAIN") ||
	       squire_hot_key_eq(entry, key_len, "GOENV") ||
	       squire_hot_key_eq(entry, key_len, "NODE_OPTIONS") ||
	       squire_hot_key_eq(entry, key_len, "NVM_BIN") ||
	       squire_hot_key_eq(entry, key_len, "NVM_DIR") ||
	       squire_hot_key_eq(entry, key_len, "PYENV_VERSION") ||
	       squire_hot_key_eq(entry, key_len, "PYTHONHOME") ||
	       squire_hot_key_eq(entry, key_len, "PYTHONPATH") ||
	       squire_hot_key_eq(entry, key_len, "VIRTUAL_ENV") ||
	       squire_hot_key_eq(entry, key_len, "CONDA_PREFIX") ||
	       squire_hot_key_eq(entry, key_len, "CARGO_HOME") ||
	       squire_hot_key_eq(entry, key_len, "RUSTUP_HOME") ||
	       squire_hot_key_eq(entry, key_len, "RUSTUP_TOOLCHAIN") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_KERNEL_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_SHIM_REAL_PATH") ||
	       (key_len >= 4 && strncmp(entry, "GIT_", 4) == 0);
}

static int squire_hot_git_metadata_env_key(const char *entry, size_t key_len) {
	return squire_hot_key_eq(entry, key_len, "PATH") ||
	       squire_hot_key_eq(entry, key_len, "HOME") ||
	       squire_hot_key_eq(entry, key_len, "XDG_CONFIG_HOME") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_KERNEL_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_SHIM_REAL_PATH") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_REAL_GIT") ||
	       squire_hot_key_eq(entry, key_len, "GIT_DIR") ||
	       squire_hot_key_eq(entry, key_len, "GIT_WORK_TREE") ||
	       squire_hot_key_eq(entry, key_len, "GIT_COMMON_DIR") ||
	       squire_hot_key_eq(entry, key_len, "GIT_NAMESPACE") ||
	       squire_hot_key_eq(entry, key_len, "GIT_INDEX_FILE") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_GLOBAL") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_SYSTEM") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_NOSYSTEM") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_COUNT") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_PARAMETERS") ||
	       squire_hot_key_has_prefix(entry, key_len, "GIT_CONFIG_KEY_") ||
	       squire_hot_key_has_prefix(entry, key_len, "GIT_CONFIG_VALUE_") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CEILING_DIRECTORIES") ||
	       squire_hot_key_eq(entry, key_len, "GIT_DISCOVERY_ACROSS_FILESYSTEM") ||
	       squire_hot_key_eq(entry, key_len, "GIT_OBJECT_DIRECTORY") ||
	       squire_hot_key_eq(entry, key_len, "GIT_ALTERNATE_OBJECT_DIRECTORIES");
}

static int squire_hot_shell_script_env_key(const char *entry, size_t key_len) {
	if (squire_hot_key_eq(entry, key_len, "GIT_PAGER") ||
	    squire_hot_key_eq(entry, key_len, "GH_PAGER")) {
		return 0;
	}
	return squire_hot_replay_env_key(entry, key_len) ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_REAL_GIT");
}

static int squire_hot_shell_plan_requires_full_env(const helper_plan *plan) {
	if (plan == NULL) {
		return 1;
	}
	for (int i = 0; i < plan->count; i++) {
		const helper_node *node = &plan->nodes[i];
		if (node->kind != HELPER_NODE_EXEC || node->argc <= 0) {
			continue;
		}
		const char *tool = base_name(node->argv[0]);
		if (tool == NULL) {
			return 1;
		}
		if (strcmp(tool, "printenv") == 0) {
			return 1;
		}
	}
	return 0;
}

static int squire_hot_shell_script_requires_full_env(const char *script) {
	if (script == NULL) {
		return 1;
	}
	helper_plan *plan = (helper_plan *)calloc(1, sizeof(*plan));
	if (plan == NULL) {
		return 1;
	}
	int requires = !helper_parse_shell_plan(script, plan) || squire_hot_shell_plan_requires_full_env(plan);
	free(plan);
	return requires;
}

#define SQUIRE_HOT_ENV_MAX_INPUTS 4096
#define SQUIRE_HOT_ENV_MAX_SELECTED 512
#define SQUIRE_HOT_ENV_TABLE_SLOTS 1024

typedef struct {
	const char *entry;
	size_t key_len;
	unsigned long long key_hash;
	unsigned char occupied;
	unsigned char seen;
} squire_hot_env_slot;

static unsigned long long squire_hot_env_key_hash(const char *key, size_t key_len) {
	unsigned long long hash = 1469598103934665603ULL;
	for (size_t i = 0; i < key_len; i++) {
		hash ^= (unsigned char)key[i];
		hash *= 1099511628211ULL;
	}
	return hash;
}

static squire_hot_env_slot *squire_hot_env_find_slot(
	squire_hot_env_slot table[SQUIRE_HOT_ENV_TABLE_SLOTS],
	const char *key,
	size_t key_len,
	unsigned long long key_hash
) {
	size_t index = (size_t)key_hash & (SQUIRE_HOT_ENV_TABLE_SLOTS - 1);
	for (size_t probe = 0; probe < SQUIRE_HOT_ENV_TABLE_SLOTS; probe++) {
		squire_hot_env_slot *slot = &table[(index + probe) & (SQUIRE_HOT_ENV_TABLE_SLOTS - 1)];
		if (!slot->occupied ||
		    (slot->key_hash == key_hash && slot->key_len == key_len &&
		     memcmp(slot->entry, key, key_len) == 0)) {
			return slot;
		}
	}
	return NULL;
}

static void squire_hot_trace_env_mismatch(const char *entry, size_t key_len, const char *reason) {
	char key[128];
	if (key_len == 0 || key_len >= sizeof(key)) {
		squire_hot_trace(reason);
		return;
	}
	memcpy(key, entry, key_len);
	key[key_len] = '\0';
	char msg[256];
	snprintf(msg, sizeof(msg), "env mismatch key=%s reason=%s", key, reason);
	squire_hot_trace(msg);
}

/*
 * Codex normally forwards the process environment without reordering it.  In
 * that common case an exact filtered sequence comparison proves compatibility
 * without building the order-independent table below.  A different ordering
 * is not a miss: it falls through to the full set comparison.
 */
static int squire_hot_env_sequences_equal_for(
	int envc,
	const char *const *env,
	int (*selected)(const char *, size_t)
) {
	if (envc < 0 || envc > SQUIRE_HOT_ENV_MAX_INPUTS || (envc > 0 && env == NULL)) {
		return -1;
	}
	int expected_index = 0;
	size_t actual_index = 0;
	size_t expected_selected = 0;
	size_t actual_selected = 0;
	for (;;) {
		const char *expected = NULL;
		while (expected_index < envc) {
			const char *entry = env[expected_index++];
			if (entry == NULL) {
				return -1;
			}
			const char *equals = strchr(entry, '=');
			if (equals == NULL) {
				return -1;
			}
			size_t key_len = (size_t)(equals - entry);
			if (selected(entry, key_len)) {
				if (key_len == 0 || key_len >= 128 || ++expected_selected > SQUIRE_HOT_ENV_MAX_SELECTED) {
					return -1;
				}
				expected = entry;
				break;
			}
		}

		const char *actual = NULL;
		while (environ != NULL) {
			if (actual_index > SQUIRE_HOT_ENV_MAX_INPUTS) {
				return -1;
			}
			const char *entry = environ[actual_index];
			if (entry == NULL) {
				break;
			}
			actual_index++;
			const char *equals = strchr(entry, '=');
			size_t key_len = equals == NULL ? strlen(entry) : (size_t)(equals - entry);
			if (selected(entry, key_len)) {
				if (equals == NULL || key_len == 0 || key_len >= 128 || ++actual_selected > SQUIRE_HOT_ENV_MAX_SELECTED) {
					return -1;
				}
				actual = entry;
				break;
			}
		}

		if (expected == NULL || actual == NULL) {
			return expected == NULL && actual == NULL;
		}
		if (strcmp(expected, actual) != 0) {
			return 0;
		}
	}
}

static int squire_hot_env_compatible_for(int envc, const char *const *env, int (*selected)(const char *, size_t)) {
	int sequence_equal = squire_hot_env_sequences_equal_for(envc, env, selected);
	if (sequence_equal != 0) {
		return sequence_equal > 0;
	}
	if (envc < 0 || envc > SQUIRE_HOT_ENV_MAX_INPUTS || (envc > 0 && env == NULL)) {
		return 0;
	}
	squire_hot_env_slot table[SQUIRE_HOT_ENV_TABLE_SLOTS];
	memset(table, 0, sizeof(table));
	size_t selected_count = 0;
	for (int i = 0; i < envc; i++) {
		const char *entry = env[i];
		if (entry == NULL) {
			return 0;
		}
		const char *equals = strchr(entry, '=');
		if (equals == NULL) {
			return 0;
		}
		size_t key_len = (size_t)(equals - entry);
		if (!selected(entry, key_len)) {
			continue;
		}
		if (key_len == 0 || key_len >= 128 || selected_count >= SQUIRE_HOT_ENV_MAX_SELECTED) {
			return 0;
		}
		unsigned long long key_hash = squire_hot_env_key_hash(entry, key_len);
		squire_hot_env_slot *slot = squire_hot_env_find_slot(table, entry, key_len, key_hash);
		if (slot == NULL || slot->occupied) {
			squire_hot_trace_env_mismatch(entry, key_len, "duplicate-expected");
			return 0;
		}
		slot->entry = entry;
		slot->key_len = key_len;
		slot->key_hash = key_hash;
		slot->occupied = 1;
		selected_count++;
	}

	size_t actual_count = 0;
	if (environ != NULL) {
		for (size_t i = 0; environ[i] != NULL; i++) {
			if (i >= SQUIRE_HOT_ENV_MAX_INPUTS) {
				return 0;
			}
			const char *entry = environ[i];
			const char *equals = strchr(entry, '=');
			size_t key_len = equals == NULL ? strlen(entry) : (size_t)(equals - entry);
			if (!selected(entry, key_len)) {
				continue;
			}
			if (equals == NULL || key_len == 0 || key_len >= 128) {
				return 0;
			}
			unsigned long long key_hash = squire_hot_env_key_hash(entry, key_len);
			squire_hot_env_slot *slot = squire_hot_env_find_slot(table, entry, key_len, key_hash);
			if (slot == NULL || !slot->occupied) {
				squire_hot_trace_env_mismatch(entry, key_len, "missing-expected");
				return 0;
			}
			if (slot->seen || strcmp(slot->entry, entry) != 0) {
				squire_hot_trace_env_mismatch(entry, key_len, slot->seen ? "duplicate-actual" : "value");
				return 0;
			}
			slot->seen = 1;
			actual_count++;
		}
	}
	return actual_count == selected_count;
}

static int squire_hot_env_compatible(int envc, const char *const *env) {
	return squire_hot_env_compatible_for(envc, env, squire_hot_replay_env_key);
}

static int squire_hot_all_env_key(const char *entry, size_t key_len) {
	(void)entry;
	return key_len > 0;
}

static int squire_hot_full_env_compatible(int envc, const char *const *env) {
	return squire_hot_env_compatible_for(envc, env, squire_hot_all_env_key);
}

static int squire_hot_git_metadata_env_compatible(int envc, const char *const *env) {
	return squire_hot_env_compatible_for(envc, env, squire_hot_git_metadata_env_key);
}

static int squire_hot_shell_script_env_compatible(const char *script, int envc, const char *const *env) {
	if (squire_hot_shell_script_requires_full_env(script)) {
		return squire_hot_env_compatible(envc, env);
	}
	return squire_hot_env_compatible_for(envc, env, squire_hot_shell_script_env_key);
}

static int squire_hot_shell_script_index(int argc, const char *const *argv) {
	if (argc < 3 || argv == NULL || argv[0] == NULL) {
		return -1;
	}
	const char *shell = base_name(argv[0]);
	if (shell == NULL ||
	    (strcmp(shell, "sh") != 0 && strcmp(shell, "bash") != 0 && strcmp(shell, "zsh") != 0)) {
		return -1;
	}
	if (argv[1] != NULL && (strcmp(argv[1], "-c") == 0 || strcmp(argv[1], "-lc") == 0)) {
		return 2;
	}
	if (argc >= 4 && argv[1] != NULL && argv[2] != NULL &&
	    strcmp(argv[1], "-l") == 0 && strcmp(argv[2], "-c") == 0) {
		return 3;
	}
	return -1;
}

static int squire_hot_direct_requires_full_env(int argc, const char *const *argv) {
	if (argc <= 0 || argv == NULL || argv[0] == NULL) {
		return 1;
	}
	const char *tool = base_name(argv[0]);
	if (tool == NULL) {
		return 1;
	}
	return strcmp(tool, "printenv") == 0;
}

static int squire_hot_is_git_metadata_argv(int argc, const char *const *argv) {
	if (argv == NULL || argc < 3 || argv[0] == NULL || argv[1] == NULL || argv[2] == NULL) {
		return 0;
	}
	if (argc == 3 &&
	    strcmp(base_name(argv[0]), "git") == 0 &&
	    strcmp(argv[1], "branch") == 0 &&
	    strcmp(argv[2], "--show-current") == 0) {
		return 1;
	}
	if (strcmp(base_name(argv[0]), "git") != 0 || strcmp(argv[1], "rev-parse") != 0) {
		return 0;
	}
	if (argc == 3) {
		return strcmp(argv[2], "HEAD") == 0 ||
		       strcmp(argv[2], "--git-dir") == 0 ||
		       strcmp(argv[2], "--show-toplevel") == 0 ||
		       strcmp(argv[2], "--is-inside-work-tree") == 0;
	}
	return argc == 4 && argv[3] != NULL &&
	       strcmp(argv[2], "--abbrev-ref") == 0 &&
	       strcmp(argv[3], "HEAD") == 0;
}

static int squire_hot_is_git_metadata_plan(const helper_plan *plan) {
	if (plan == NULL || plan->root < 0 || plan->root >= plan->count) {
		return 0;
	}
	const helper_node *node = &plan->nodes[plan->root];
	if (node->kind != HELPER_NODE_EXEC || node->argc <= 0) {
		return 0;
	}
	const char *argv[HELPER_SHELL_MAX_ARGS];
	for (int i = 0; i < node->argc; i++) {
		argv[i] = node->argv[i];
	}
	return squire_hot_is_git_metadata_argv(node->argc, argv);
}

static int squire_hot_is_git_metadata_script(const char *script) {
	if (script == NULL) {
		return 0;
	}
	helper_plan *plan = (helper_plan *)calloc(1, sizeof(*plan));
	if (plan == NULL) {
		return 0;
	}
	int metadata = helper_parse_shell_plan(script, plan) && squire_hot_is_git_metadata_plan(plan);
	free(plan);
	return metadata;
}

static int squire_runtime_direct_candidate(const char *cwd, int argc, const char *const *argv) {
	if (cwd == NULL || argv == NULL || argc <= 0 || argc > MAX_ARGC) {
		return 0;
	}
	char *mutable_argv[MAX_ARGC];
	for (int i = 0; i < argc; i++) {
		if (argv[i] == NULL) {
			return 0;
		}
		mutable_argv[i] = (char *)argv[i];
	}
	policy_invocation inv;
	if (!normalize_invocation_at_cwd(cwd, argc, mutable_argv, &inv)) {
		return 0;
	}
	char target[PATH_BUF], flag[16];
	const char *lookup_target = NULL;
	helper_git_history_query history_query;
	return is_git_metadata(&inv) ||
	       is_git_ls_files(&inv) ||
	       is_git_status(&inv) ||
	       is_git_head_subject_log(&inv) ||
	       helper_git_history_parse(inv.argc, inv.argv, &history_query) ||
	       is_git_read_only_diff(&inv) ||
	       is_fixed_rg_repo_search(&inv) ||
	       is_bounded_rg_repo_search(&inv) ||
	       is_tool_version_probe(&inv) ||
	       command_path_lookup_target(&inv, &lookup_target) ||
	       is_static_environment_probe(&inv) ||
	       is_printenv_probe(&inv) ||
	       parse_directory_listing(&inv, target, flag) ||
	       is_file_type_candidate(&inv) ||
	       is_warm_file_candidate(&inv);
}

static int squire_runtime_filter_candidate(helper_node *node) {
	if (node == NULL || node->kind != HELPER_NODE_EXEC || node->argc <= 0) {
		return 0;
	}
	const char *name = base_name(node->argv[0]);
	if (name == NULL) {
		return 0;
	}
	int count = 0;
	line_selection selection;
	const char *pattern = NULL;
	int quiet = 0;
	return (strcmp(name, "cat") == 0 && node->argc == 1) ||
	       (strcmp(name, "head") == 0 && helper_parse_line_count_arg(node, &count) && count > 0) ||
	       (strcmp(name, "tail") == 0 && helper_parse_line_count_arg(node, &count)) ||
	       (strcmp(name, "sed") == 0 && helper_parse_stdin_sed(node, &selection)) ||
	       (strcmp(name, "grep") == 0 && helper_parse_stdin_grep(node->argc, node->argv, &pattern, &quiet)) ||
	       (strcmp(name, "wc") == 0 && node->argc == 2 && strcmp(node->argv[1], "-l") == 0) ||
	       (strcmp(name, "sort") == 0 && node->argc == 1);
}

static int squire_runtime_plan_candidate(const char *cwd, helper_plan *plan, int idx, int has_input) {
	if (plan == NULL || idx < 0 || idx >= plan->count) {
		return 0;
	}
	helper_node *node = &plan->nodes[idx];
	switch (node->kind) {
	case HELPER_NODE_EXEC:
		if (has_input) {
			return squire_runtime_filter_candidate(node);
		}
		if (node->argc == 1 && strcmp(base_name(node->argv[0]), "pwd") == 0) {
			return 1;
		}
		if (node->shell_glob_mask != 0 && strcmp(base_name(node->argv[0]), "rg") == 0) {
			for (int i = 0; i < node->argc; i++) {
				if ((node->shell_glob_mask & (UINT64_C(1) << i)) != 0 && !helper_shell_glob_arg_safe(node->argv[i])) {
					return 0;
				}
			}
			return 1;
		}
		{
			const char *argv[HELPER_SHELL_MAX_ARGS];
			for (int i = 0; i < node->argc; i++) {
				argv[i] = node->argv[i];
			}
			return squire_runtime_direct_candidate(cwd, node->argc, argv);
		}
	case HELPER_NODE_PIPE:
		return squire_runtime_plan_candidate(cwd, plan, node->left, has_input) &&
		       squire_runtime_plan_candidate(cwd, plan, node->right, 1);
	case HELPER_NODE_AND:
	case HELPER_NODE_SEQ:
		return squire_runtime_plan_candidate(cwd, plan, node->left, has_input) &&
		       squire_runtime_plan_candidate(cwd, plan, node->right, has_input);
	case HELPER_NODE_REDIR_NULL:
		return squire_runtime_plan_candidate(cwd, plan, node->left, has_input);
	default:
		return 0;
	}
}

static int squire_runtime_candidate(const char *cwd, int argc, const char *const *argv) {
	int script_idx = squire_hot_shell_script_index(argc, argv);
	if (script_idx >= 0 && script_idx < argc && argv[script_idx] != NULL) {
		helper_plan plan;
		return helper_parse_shell_plan(argv[script_idx], &plan) &&
		       squire_runtime_plan_candidate(cwd, &plan, plan.root, 0);
	}
	return squire_runtime_direct_candidate(cwd, argc, argv);
}

static void squire_hot_enqueue_plan_requests(const char *cwd, helper_plan *plan) {
	if (plan == NULL) {
		return;
	}
	for (int i = 0; i < plan->count; i++) {
		helper_node *node = &plan->nodes[i];
		if (node->kind != HELPER_NODE_EXEC || node->argc <= 0) {
			continue;
		}
		char *mutable_argv[HELPER_SHELL_MAX_ARGS];
		for (int j = 0; j < node->argc; j++) {
			mutable_argv[j] = node->argv[j];
		}
		(void)enqueue_prepare_request_at_cwd(cwd, node->argc, mutable_argv);
	}
}

int squire_hot_try_replay_command(const char *cwd, int argc, const char *const *argv, int envc, const char *const *env, squire_hot_result *out) {
	if (out == NULL || argc <= 0 || argv == NULL) {
		return 0;
	}
	memset(out, 0, sizeof(*out));
	int script_idx = squire_hot_shell_script_index(argc, argv);
	int git_metadata = squire_hot_is_git_metadata_argv(argc, argv) ||
	                   (script_idx >= 0 && script_idx < argc && squire_hot_is_git_metadata_script(argv[script_idx]));
	if (git_metadata) {
		if (!squire_hot_git_metadata_env_compatible(envc, env)) {
			squire_hot_trace("miss git-metadata-env-incompatible");
			return 0;
		}
	} else if (script_idx >= 0 && script_idx < argc && argv[script_idx] != NULL) {
		int env_ok = squire_hot_shell_script_requires_full_env(argv[script_idx])
		             ? squire_hot_full_env_compatible(envc, env)
		             : squire_hot_shell_script_env_compatible(argv[script_idx], envc, env);
		if (!env_ok) {
			squire_hot_trace("miss shell-script-env-incompatible");
			return 0;
		}
	} else if (squire_hot_direct_requires_full_env(argc, argv)) {
		if (!squire_hot_full_env_compatible(envc, env)) {
			squire_hot_trace("miss full-env-incompatible");
			return 0;
		}
	} else if (!squire_hot_env_compatible(envc, env)) {
		squire_hot_trace("miss env-incompatible");
		return 0;
	}
	if (script_idx >= 0 && script_idx < argc && argv[script_idx] != NULL) {
		squire_hot_trace("try shell script");
		int replayed = squire_hot_try_replay_script(cwd, argv[script_idx], out);
		if (!replayed) {
			helper_plan plan;
			if (helper_parse_shell_plan(argv[script_idx], &plan)) {
				squire_hot_enqueue_plan_requests(cwd, &plan);
			}
		}
		return replayed;
	}
	squire_hot_trace("try argv");
	int replayed = squire_hot_try_replay_argv(cwd, argc, argv, out);
	if (!replayed && argc <= MAX_ARGC) {
		char *mutable_argv[MAX_ARGC];
		for (int i = 0; i < argc; i++) {
			mutable_argv[i] = (char *)argv[i];
		}
		(void)enqueue_prepare_request_at_cwd(cwd, argc, mutable_argv);
	}
	return replayed;
}

uint32_t squire_runtime_abi_version(void) {
	return SQUIRE_RUNTIME_ABI_VERSION;
}

int squire_runtime_try_execute(const char *cwd, int argc, const char *const *argv, int envc, const char *const *env, squire_runtime_result *out) {
	int script_idx = squire_hot_shell_script_index(argc, argv);
	if (script_idx >= 0 && script_idx < argc && argv[script_idx] != NULL) {
		helper_plan *plan = (helper_plan *)calloc(1, sizeof(*plan));
		if (plan == NULL) {
			if (out != NULL) {
				memset(out, 0, sizeof(*out));
			}
			return SQUIRE_RUNTIME_MISS;
		}
		if (!helper_parse_shell_plan(argv[script_idx], plan) ||
		    !squire_runtime_plan_candidate(cwd, plan, plan->root, 0)) {
			free(plan);
			if (out != NULL) {
				memset(out, 0, sizeof(*out));
			}
			return SQUIRE_RUNTIME_UNSUPPORTED;
		}

		int env_ok;
		if (squire_hot_is_git_metadata_plan(plan)) {
			env_ok = squire_hot_git_metadata_env_compatible(envc, env);
		} else if (squire_hot_shell_plan_requires_full_env(plan)) {
			env_ok = squire_hot_full_env_compatible(envc, env);
		} else {
			env_ok = squire_hot_env_compatible_for(envc, env, squire_hot_shell_script_env_key);
		}
		if (!env_ok) {
			free(plan);
			if (out != NULL) {
				memset(out, 0, sizeof(*out));
			}
			return SQUIRE_RUNTIME_MISS;
		}

		int replayed = squire_hot_try_replay_plan(cwd, plan, out);
		if (!replayed) {
			squire_hot_enqueue_plan_requests(cwd, plan);
		}
		free(plan);
		return replayed ? SQUIRE_RUNTIME_HIT : SQUIRE_RUNTIME_MISS;
	}
	if (!squire_runtime_candidate(cwd, argc, argv)) {
		if (out != NULL) {
			memset(out, 0, sizeof(*out));
		}
		return SQUIRE_RUNTIME_UNSUPPORTED;
	}
	return squire_hot_try_replay_command(cwd, argc, argv, envc, env, out) == 1
	       ? SQUIRE_RUNTIME_HIT
	       : SQUIRE_RUNTIME_MISS;
}

void squire_hot_record_replay(squire_hot_result *result) {
	if (result == NULL || result->handle == NULL) {
		return;
	}
	squire_hot_handle *handle = (squire_hot_handle *)result->handle;
	if (handle->kind == SQUIRE_HOT_HANDLE_EXACT) {
		record_hot_replay_event(handle->exact.store_root, (long long)handle->exact.native_wall_ms, handle->exact.replay_start_ns);
	} else if (handle->kind == SQUIRE_HOT_HANDLE_SCRIPT && handle->script.replay_sources > 0) {
		const char *proof = handle->script.used_current_file ? HOT_CLIENT_PROOF_C_CURRENT_FILE : HOT_CLIENT_PROOF_C_MMAP;
		record_hot_replay_event_kind(handle->script.replay_store_root, proof, handle->script.native_wall_ms,
		                             handle->script.replay_start_ns);
	}
}

void squire_runtime_record_hit(squire_runtime_result *result) {
	squire_hot_record_replay(result);
}

void squire_hot_release(squire_hot_result *result) {
	if (result == NULL || result->handle == NULL) {
		return;
	}
	squire_hot_handle *handle = (squire_hot_handle *)result->handle;
	if (handle->kind == SQUIRE_HOT_HANDLE_EXACT) {
		release_prepared_exact_replay(&handle->exact);
	} else if (handle->kind == SQUIRE_HOT_HANDLE_SCRIPT) {
		helper_result_free(&handle->script);
	}
	free(handle);
	memset(result, 0, sizeof(*result));
}

void squire_runtime_release(squire_runtime_result *result) {
	squire_hot_release(result);
}
