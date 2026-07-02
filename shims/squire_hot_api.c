#define SQUIRE_MMAP_EMBEDDED
#define SQUIRE_MMAP_NO_MAIN 1
#define SQUIRE_PRELOAD_HELPER_NO_MAIN

#include "squire_hot_api.h"

#include <stdlib.h>
#include <string.h>

#include "squire_preload_helper.c"

typedef enum {
	SQUIRE_HOT_HANDLE_EXACT = 1,
	SQUIRE_HOT_HANDLE_SCRIPT = 2,
} squire_hot_handle_kind;

typedef struct {
	squire_hot_handle_kind kind;
	prepared_exact_replay exact;
	helper_result script;
} squire_hot_handle;

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
		if (!helper_prepare_warm_file_at_cwd(cwd, argc, mutable_argv, &handle->script, now_monotonic_ns())) {
			free(handle);
			return 0;
		}
		squire_hot_fill_script(out, handle);
		return 1;
	}
	squire_hot_fill_exact(out, handle);
	return 1;
}

int squire_hot_try_replay_script(const char *cwd, const char *script, squire_hot_result *out) {
	if (out == NULL || script == NULL || script[0] == '\0') {
		return 0;
	}
	memset(out, 0, sizeof(*out));
	squire_hot_handle *handle = (squire_hot_handle *)calloc(1, sizeof(*handle));
	if (handle == NULL) {
		return 0;
	}
	handle->kind = SQUIRE_HOT_HANDLE_SCRIPT;
	if (!helper_eval_shell_ir_at_cwd(cwd, script, &handle->script, now_monotonic_ns())) {
		free(handle);
		return 0;
	}
	squire_hot_fill_script(out, handle);
	return 1;
}

static int squire_hot_key_eq(const char *entry, size_t key_len, const char *key) {
	return strlen(key) == key_len && strncmp(entry, key, key_len) == 0;
}

static int squire_hot_sensitive_env_key(const char *entry, size_t key_len) {
	return squire_hot_key_eq(entry, key_len, "PATH") ||
	       squire_hot_key_eq(entry, key_len, "HOME") ||
	       squire_hot_key_eq(entry, key_len, "LANG") ||
	       squire_hot_key_eq(entry, key_len, "LC_ALL") ||
	       squire_hot_key_eq(entry, key_len, "LC_CTYPE") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_KERNEL_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_SHIM_REAL_PATH") ||
	       (key_len >= 4 && strncmp(entry, "GIT_", 4) == 0);
}

static int squire_hot_env_compatible(int envc, const char *const *env) {
	if (envc < 0 || (envc > 0 && env == NULL)) {
		return 0;
	}
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
		if (!squire_hot_sensitive_env_key(entry, key_len)) {
			continue;
		}
		char key[128];
		if (key_len == 0 || key_len >= sizeof(key)) {
			return 0;
		}
		memcpy(key, entry, key_len);
		key[key_len] = '\0';
		const char *actual = getenv(key);
		const char *expected = equals + 1;
		if (actual == NULL || strcmp(actual, expected) != 0) {
			return 0;
		}
	}
	return 1;
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

int squire_hot_try_replay_command(const char *cwd, int argc, const char *const *argv, int envc, const char *const *env, squire_hot_result *out) {
	if (out == NULL || argc <= 0 || argv == NULL) {
		return 0;
	}
	memset(out, 0, sizeof(*out));
	if (!squire_hot_env_compatible(envc, env)) {
		return 0;
	}
	int script_idx = squire_hot_shell_script_index(argc, argv);
	if (script_idx >= 0 && script_idx < argc && argv[script_idx] != NULL) {
		return squire_hot_try_replay_script(cwd, argv[script_idx], out);
	}
	return squire_hot_try_replay_argv(cwd, argc, argv, out);
}

void squire_hot_record_replay(squire_hot_result *result) {
	if (result == NULL || result->handle == NULL) {
		return;
	}
	squire_hot_handle *handle = (squire_hot_handle *)result->handle;
	if (handle->kind == SQUIRE_HOT_HANDLE_EXACT) {
		record_hot_replay_event(handle->exact.store_root, (long long)handle->exact.native_wall_ms, handle->exact.replay_start_ns);
	}
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
