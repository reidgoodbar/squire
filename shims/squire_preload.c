// Experimental scoped preload transport for Squire's mmap hot snapshot.
//
// This library is loaded only inside an explicit `squire session`. It
// intercepts already-chosen exec calls, tries the same exact mmap proof used
// by the embedded mmap proof engine, and falls through to the native exec
// implementation on every miss. It never installs or depends on PATH shims.

#define _GNU_SOURCE

#include <dlfcn.h>
#include <errno.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>
#if defined(__APPLE__)
#include <mach-o/dyld.h>
#endif

extern char **environ;

#define SQUIRE_MMAP_EMBEDDED 1
#include "squire_mmap_shim.c"

static __thread int squire_preload_guard;
static int squire_preload_trace_enabled;
static int squire_preload_active;
static int squire_preload_require_hit;

static int env_has_prefix(const char *entry, const char *prefix);
static int parsed_shell_replay_env_compatible(int argc, char *const argv[], char *const envp[]);

static void preload_trace(const char *event, const char *path) {
	if (!squire_preload_trace_enabled) {
		return;
	}
	write_all(STDERR_FILENO, "squire preload trace: ", 22);
	write_all(STDERR_FILENO, event, strlen(event));
	if (path != NULL) {
		write_all(STDERR_FILENO, " ", 1);
		write_all(STDERR_FILENO, path, strlen(path));
	}
	write_all(STDERR_FILENO, "\n", 1);
}

static void preload_trace_shell_argv(const char *event, const char *path, char *const argv[]) {
	if (!squire_preload_trace_enabled) {
		return;
	}
	write_all(STDERR_FILENO, "squire preload trace: ", 22);
	write_all(STDERR_FILENO, event, strlen(event));
	if (path != NULL) {
		write_all(STDERR_FILENO, " path=", 6);
		write_all(STDERR_FILENO, path, strlen(path));
	}
	for (int i = 0; argv != NULL && argv[i] != NULL && i < 4; i++) {
		char label[16];
		int n = snprintf(label, sizeof(label), " argv%d=", i);
		if (n > 0 && n < (int)sizeof(label)) {
			write_all(STDERR_FILENO, label, (size_t)n);
			write_all(STDERR_FILENO, argv[i], strlen(argv[i]));
		}
	}
	write_all(STDERR_FILENO, "\n", 1);
}

static int env_value_contains(const char *name, const char *needle) {
	const char *value = getenv(name);
	return value != NULL && needle != NULL && strstr(value, needle) != NULL;
}

static int preload_active(void) {
	return squire_preload_active;
}

static int require_hit(void) {
	return squire_preload_require_hit;
}

static int shell_ir_enabled(void) {
#ifndef SQUIRE_ENABLE_EXPERIMENTAL_SHELL_IR
	return 0;
#else
	const char *value = getenv("SQUIRE_SHELL_IR_ENABLE");
	return value != NULL && value[0] != '\0' && strcmp(value, "0") != 0;
#endif
}

static int preload_tool_candidate(const char *path, char *const argv[]) {
	if (path == NULL || path[0] == '\0' || argv == NULL || argv[0] == NULL) {
		return 0;
	}
	const char *tool = base_name(argv[0]);
	if (tool == NULL || tool[0] == '\0') {
		tool = base_name(path);
	}
	if (strcmp(tool, "git") == 0 ||
	    strcmp(tool, "which") == 0 ||
	    strcmp(tool, "command") == 0 ||
	    strcmp(tool, "ls") == 0 ||
	    strcmp(tool, "file") == 0 ||
	    strcmp(tool, "grep") == 0 ||
	    strcmp(tool, "rg") == 0 ||
	    strcmp(tool, "printenv") == 0 ||
	    strcmp(tool, "whoami") == 0 ||
	    strcmp(tool, "uname") == 0 ||
	    strcmp(tool, "id") == 0 ||
	    strcmp(tool, "hostname") == 0 ||
	    strcmp(tool, "go") == 0 ||
	    strcmp(tool, "node") == 0 ||
	    strcmp(tool, "npm") == 0 ||
	    strcmp(tool, "pnpm") == 0 ||
	    strcmp(tool, "yarn") == 0 ||
	    strcmp(tool, "python") == 0 ||
	    strcmp(tool, "python3") == 0 ||
	    strcmp(tool, "pip") == 0 ||
	    strcmp(tool, "pip3") == 0 ||
	    strcmp(tool, "cargo") == 0 ||
	    strcmp(tool, "rustc") == 0 ||
	    strcmp(tool, "make") == 0) {
		return 1;
	}
	if ((strcmp(tool, "cat") == 0 || strcmp(tool, "sed") == 0 || strcmp(tool, "head") == 0 || strcmp(tool, "tail") == 0) && warm_file_replay_enabled()) {
		return 1;
	}
	return 0;
}

static int count_argv(char *const argv[]) {
	int argc = 0;
	if (argv == NULL) {
		return 0;
	}
	while (argv[argc] != NULL) {
		argc++;
		if (argc > MAX_ARGC) {
			return 0;
		}
	}
	return argc;
}

static int shell_name_matches(const char *name) {
	if (name == NULL || name[0] == '\0') {
		return 0;
	}
	if (name[0] == '-') {
		name++;
	}
	return strcmp(name, "sh") == 0 || strcmp(name, "bash") == 0 || strcmp(name, "zsh") == 0;
}

static int is_shell_launcher_name(const char *path, char *const argv[]) {
	const char *path_name = base_name(path);
	if (shell_name_matches(path_name)) {
		return 1;
	}
	if (argv != NULL && argv[0] != NULL) {
		return shell_name_matches(base_name(argv[0]));
	}
	return 0;
}

static const char *shell_command_arg(char *const argv[]) {
	if (argv == NULL || argv[0] == NULL) {
		return NULL;
	}
	if ((strcmp(argv[0], "-c") == 0 || strcmp(argv[0], "-lc") == 0) && argv[1] != NULL) {
		return argv[1];
	}
	if (argv[1] == NULL) {
		return NULL;
	}
	if (strcmp(argv[1], "-c") == 0 || strcmp(argv[1], "-lc") == 0) {
		if (argv[2] == NULL) {
			return NULL;
		}
		return argv[2];
	}
	if (strcmp(argv[1], "-l") == 0 && argv[2] != NULL && strcmp(argv[2], "-c") == 0) {
		if (argv[3] == NULL) {
			return NULL;
		}
		return argv[3];
	}
	return NULL;
}

static const char *shell_command_flag(char *const argv[]) {
	if (argv == NULL || argv[0] == NULL) {
		return NULL;
	}
	if ((strcmp(argv[0], "-c") == 0 || strcmp(argv[0], "-lc") == 0) && argv[1] != NULL) {
		return argv[0];
	}
	if (argv[1] == NULL) {
		return NULL;
	}
	if (strcmp(argv[1], "-c") == 0 || strcmp(argv[1], "-lc") == 0) {
		return argv[1];
	}
	if (strcmp(argv[1], "-l") == 0 && argv[2] != NULL && strcmp(argv[2], "-c") == 0) {
		return argv[2];
	}
	return NULL;
}

static int shell_command_composition_candidate(const char *command) {
	if (command == NULL || command[0] == '\0') {
		return 0;
	}
	int has_operator = 0;
	for (const unsigned char *p = (const unsigned char *)command; *p != '\0'; p++) {
		switch (*p) {
		case '$':
		case '`':
		case '!':
		case '*':
		case '?':
		case '<':
		case '{':
		case '}':
		case '[':
		case ']':
			return 0;
		case '|':
		case ';':
		case '(':
		case ')':
			has_operator = 1;
			break;
		case '&':
			if (p[1] == '&') {
				has_operator = 1;
				p++;
				break;
			}
			return 0;
		case '>':
			if (strncmp((const char *)p, ">/dev/null", strlen(">/dev/null")) == 0 ||
			    strncmp((const char *)p, "> /dev/null", strlen("> /dev/null")) == 0) {
				has_operator = 1;
				break;
			}
			return 0;
		default:
			break;
		}
	}
	return has_operator;
}

static int shell_token_byte_allowed(unsigned char c) {
	if (c <= 0x20 || c >= 0x7f) {
		return 0;
	}
	switch (c) {
	case '"':
	case '\'':
	case '\\':
	case '$':
	case '|':
	case '&':
	case ';':
	case '<':
	case '>':
	case '(':
	case ')':
	case '{':
	case '}':
	case '[':
	case ']':
	case '*':
	case '?':
	case '~':
	case '`':
	case '!':
	case '#':
		return 0;
	default:
		return 1;
	}
}

static int parse_simple_shell_command(const char *command, int *argc_out, char **argv_out, char storage[MAX_ARGC][PATH_BUF]) {
	if (command == NULL || argc_out == NULL || argv_out == NULL) {
		return 0;
	}
	int argc = 0;
	const unsigned char *p = (const unsigned char *)command;
	while (*p != '\0') {
		while (*p == ' ' || *p == '\t') {
			p++;
		}
		if (*p == '\0') {
			break;
		}
		if (argc >= MAX_ARGC - 1) {
			return 0;
		}
		size_t len = 0;
		while (p[len] != '\0' && p[len] != ' ' && p[len] != '\t') {
			if (!shell_token_byte_allowed(p[len])) {
				return 0;
			}
			len++;
			if (len >= PATH_BUF) {
				return 0;
			}
		}
		if (len == 0) {
			return 0;
		}
		memcpy(storage[argc], p, len);
		storage[argc][len] = '\0';
		argv_out[argc] = storage[argc];
		argc++;
		p += len;
	}
	if (argc <= 0) {
		return 0;
	}
	if (strchr(argv_out[0], '=') != NULL) {
		return 0;
	}
	argv_out[argc] = NULL;
	*argc_out = argc;
	return 1;
}

#define SHELL_IR_MAX_TOKENS 96
#define SHELL_IR_MAX_NODES 64
#define SHELL_IR_MAX_ARGS 16
#define SHELL_IR_WORD_BUF 512

typedef enum {
	SHELL_TOKEN_WORD = 1,
	SHELL_TOKEN_PIPE,
	SHELL_TOKEN_AND,
	SHELL_TOKEN_SEMI,
	SHELL_TOKEN_LPAREN,
	SHELL_TOKEN_RPAREN,
	SHELL_TOKEN_REDIR_NULL,
} shell_token_kind;

typedef struct {
	shell_token_kind kind;
	char text[SHELL_IR_WORD_BUF];
	int fd;
} shell_token;

typedef enum {
	SHELL_NODE_EXEC = 1,
	SHELL_NODE_PIPE,
	SHELL_NODE_AND,
	SHELL_NODE_SEQ,
	SHELL_NODE_REDIR_NULL,
} shell_node_kind;

typedef struct {
	shell_node_kind kind;
	int left;
	int right;
	int fd;
	int argc;
	char argv[SHELL_IR_MAX_ARGS][SHELL_IR_WORD_BUF];
} shell_ir_node;

typedef struct {
	shell_ir_node nodes[SHELL_IR_MAX_NODES];
	int count;
	int root;
} shell_ir_plan;

typedef struct {
	shell_token tokens[SHELL_IR_MAX_TOKENS];
	int count;
	int pos;
	shell_ir_plan plan;
} shell_ir_parser;

typedef struct {
	byte_buf stdout_buf;
	byte_buf stderr_buf;
	int exit_code;
} shell_eval_result;

static int shell_word_byte_allowed(unsigned char c) {
	if (c <= 0x1f || c >= 0x7f) {
		return 0;
	}
	switch (c) {
	case '\\':
	case '$':
	case '`':
	case '!':
	case '*':
	case '?':
	case '~':
	case '<':
	case '{':
	case '}':
	case '[':
	case ']':
	case '#':
		return 0;
	default:
		return 1;
	}
}

static int shell_word_meta(unsigned char c) {
	return c == '\0' || c == ' ' || c == '\t' || c == '\n' ||
	       c == '|' || c == '&' || c == ';' || c == '(' || c == ')' ||
	       c == '<' || c == '>';
}

static int shell_token_add(shell_token *tokens, int *count, shell_token_kind kind, const char *text, size_t text_len, int fd) {
	if (*count >= SHELL_IR_MAX_TOKENS - 1 || text_len >= SHELL_IR_WORD_BUF) {
		return 0;
	}
	tokens[*count].kind = kind;
	tokens[*count].fd = fd;
	if (text != NULL && text_len > 0) {
		memcpy(tokens[*count].text, text, text_len);
		tokens[*count].text[text_len] = '\0';
	} else {
		tokens[*count].text[0] = '\0';
	}
	(*count)++;
	return 1;
}

static int shell_tokenize_redir_null(const unsigned char **cursor, shell_token *tokens, int *count, int fd) {
	const unsigned char *p = *cursor;
	if (*p != '>') {
		return 0;
	}
	p++;
	while (*p == ' ' || *p == '\t') {
		p++;
	}
	if (strncmp((const char *)p, "/dev/null", strlen("/dev/null")) != 0) {
		return 0;
	}
	p += strlen("/dev/null");
	if (!shell_word_meta(*p)) {
		return 0;
	}
	*cursor = p;
	return shell_token_add(tokens, count, SHELL_TOKEN_REDIR_NULL, NULL, 0, fd);
}

static int shell_tokenize(const char *command, shell_token *tokens, int *count) {
	if (command == NULL || tokens == NULL || count == NULL) {
		return 0;
	}
	*count = 0;
	const unsigned char *p = (const unsigned char *)command;
	while (*p != '\0') {
		while (*p == ' ' || *p == '\t' || *p == '\n') {
			p++;
		}
		if (*p == '\0') {
			break;
		}
		if (*p == '|') {
			if (p[1] == '|') {
				return 0;
			}
			if (!shell_token_add(tokens, count, SHELL_TOKEN_PIPE, NULL, 0, 0)) {
				return 0;
			}
			p++;
			continue;
		}
		if (*p == '&') {
			if (p[1] != '&') {
				return 0;
			}
			if (!shell_token_add(tokens, count, SHELL_TOKEN_AND, NULL, 0, 0)) {
				return 0;
			}
			p += 2;
			continue;
		}
		if (*p == ';') {
			if (!shell_token_add(tokens, count, SHELL_TOKEN_SEMI, NULL, 0, 0)) {
				return 0;
			}
			p++;
			continue;
		}
		if (*p == '(') {
			if (!shell_token_add(tokens, count, SHELL_TOKEN_LPAREN, NULL, 0, 0)) {
				return 0;
			}
			p++;
			continue;
		}
		if (*p == ')') {
			if (!shell_token_add(tokens, count, SHELL_TOKEN_RPAREN, NULL, 0, 0)) {
				return 0;
			}
			p++;
			continue;
		}
		if (*p == '>') {
			if (!shell_tokenize_redir_null(&p, tokens, count, 1)) {
				return 0;
			}
			continue;
		}
		if ((*p == '1' || *p == '2') && p[1] == '>') {
			int fd = *p == '2' ? 2 : 1;
			p++;
			if (!shell_tokenize_redir_null(&p, tokens, count, fd)) {
				return 0;
			}
			continue;
		}
		char word[SHELL_IR_WORD_BUF];
		size_t len = 0;
		while (!shell_word_meta(*p)) {
			if (*p == '\'' || *p == '"') {
				unsigned char quote = *p++;
				while (*p != '\0' && *p != quote) {
					if (!shell_word_byte_allowed(*p)) {
						return 0;
					}
					if (quote == '"' && (*p == '$' || *p == '`' || *p == '\\' || *p == '!')) {
						return 0;
					}
					if (len + 1 >= sizeof(word)) {
						return 0;
					}
					word[len++] = (char)*p++;
				}
				if (*p != quote) {
					return 0;
				}
				p++;
				continue;
			}
			if (!shell_word_byte_allowed(*p) || *p == '\'' || *p == '"') {
				return 0;
			}
			if (len + 1 >= sizeof(word)) {
				return 0;
			}
			word[len++] = (char)*p++;
		}
		if (len == 0 || !shell_token_add(tokens, count, SHELL_TOKEN_WORD, word, len, 0)) {
			return 0;
		}
	}
	return *count > 0;
}

static int shell_ir_add_node(shell_ir_parser *p, shell_node_kind kind, int left, int right, int fd) {
	if (p->plan.count >= SHELL_IR_MAX_NODES) {
		return -1;
	}
	int idx = p->plan.count++;
	memset(&p->plan.nodes[idx], 0, sizeof(p->plan.nodes[idx]));
	p->plan.nodes[idx].kind = kind;
	p->plan.nodes[idx].left = left;
	p->plan.nodes[idx].right = right;
	p->plan.nodes[idx].fd = fd;
	return idx;
}

static shell_token *shell_ir_peek(shell_ir_parser *p) {
	if (p->pos >= p->count) {
		return NULL;
	}
	return &p->tokens[p->pos];
}

static int shell_ir_accept(shell_ir_parser *p, shell_token_kind kind) {
	shell_token *tok = shell_ir_peek(p);
	if (tok == NULL || tok->kind != kind) {
		return 0;
	}
	p->pos++;
	return 1;
}

static int shell_ir_parse_sequence(shell_ir_parser *p, int *node_out);

static int shell_ir_parse_primary(shell_ir_parser *p, int *node_out) {
	shell_token *tok = shell_ir_peek(p);
	if (tok == NULL) {
		return 0;
	}
	int node = -1;
	if (shell_ir_accept(p, SHELL_TOKEN_LPAREN)) {
		if (!shell_ir_parse_sequence(p, &node) || !shell_ir_accept(p, SHELL_TOKEN_RPAREN)) {
			return 0;
		}
	} else if (tok->kind == SHELL_TOKEN_WORD) {
		node = shell_ir_add_node(p, SHELL_NODE_EXEC, -1, -1, 0);
		if (node < 0) {
			return 0;
		}
		while ((tok = shell_ir_peek(p)) != NULL && tok->kind == SHELL_TOKEN_WORD) {
			if (p->plan.nodes[node].argc >= SHELL_IR_MAX_ARGS - 1) {
				return 0;
			}
			snprintf(p->plan.nodes[node].argv[p->plan.nodes[node].argc], SHELL_IR_WORD_BUF, "%s", tok->text);
			p->plan.nodes[node].argc++;
			p->pos++;
		}
		if (p->plan.nodes[node].argc <= 0 || strchr(p->plan.nodes[node].argv[0], '=') != NULL) {
			return 0;
		}
	} else {
		return 0;
	}
	while ((tok = shell_ir_peek(p)) != NULL && tok->kind == SHELL_TOKEN_REDIR_NULL) {
		p->pos++;
		node = shell_ir_add_node(p, SHELL_NODE_REDIR_NULL, node, -1, tok->fd);
		if (node < 0) {
			return 0;
		}
	}
	*node_out = node;
	return 1;
}

static int shell_ir_parse_pipeline(shell_ir_parser *p, int *node_out) {
	int node = -1;
	if (!shell_ir_parse_primary(p, &node)) {
		return 0;
	}
	while (shell_ir_accept(p, SHELL_TOKEN_PIPE)) {
		int right = -1;
		if (!shell_ir_parse_primary(p, &right)) {
			return 0;
		}
		node = shell_ir_add_node(p, SHELL_NODE_PIPE, node, right, 0);
		if (node < 0) {
			return 0;
		}
	}
	*node_out = node;
	return 1;
}

static int shell_ir_parse_and(shell_ir_parser *p, int *node_out) {
	int node = -1;
	if (!shell_ir_parse_pipeline(p, &node)) {
		return 0;
	}
	while (shell_ir_accept(p, SHELL_TOKEN_AND)) {
		int right = -1;
		if (!shell_ir_parse_pipeline(p, &right)) {
			return 0;
		}
		node = shell_ir_add_node(p, SHELL_NODE_AND, node, right, 0);
		if (node < 0) {
			return 0;
		}
	}
	*node_out = node;
	return 1;
}

static int shell_ir_parse_sequence(shell_ir_parser *p, int *node_out) {
	int node = -1;
	if (!shell_ir_parse_and(p, &node)) {
		return 0;
	}
	while (shell_ir_accept(p, SHELL_TOKEN_SEMI)) {
		if (shell_ir_peek(p) == NULL || shell_ir_peek(p)->kind == SHELL_TOKEN_RPAREN) {
			break;
		}
		int right = -1;
		if (!shell_ir_parse_and(p, &right)) {
			return 0;
		}
		node = shell_ir_add_node(p, SHELL_NODE_SEQ, node, right, 0);
		if (node < 0) {
			return 0;
		}
	}
	*node_out = node;
	return 1;
}

static int shell_ir_parse(const char *command, shell_ir_plan *plan) {
	if (command == NULL || plan == NULL) {
		return 0;
	}
	shell_ir_parser *p = (shell_ir_parser *)calloc(1, sizeof(*p));
	if (p == NULL) {
		return 0;
	}
	int root = -1;
	if (!shell_tokenize(command, p->tokens, &p->count) ||
	    !shell_ir_parse_sequence(p, &root) ||
	    p->pos != p->count ||
	    root < 0) {
		free(p);
		return 0;
	}
	p->plan.root = root;
	*plan = p->plan;
	free(p);
	return 1;
}

static void shell_eval_free(shell_eval_result *res) {
	if (res == NULL) {
		return;
	}
	bytes_free(&res->stdout_buf);
	bytes_free(&res->stderr_buf);
	memset(res, 0, sizeof(*res));
}

static int shell_eval_append(byte_buf *dst, const byte_buf *src) {
	if (src == NULL || src->len == 0) {
		return 1;
	}
	return bytes_append(dst, src->data, src->len);
}

static int shell_eval_copy_bytes(byte_buf *dst, const unsigned char *data, uint32_t len) {
	if (len == 0) {
		return 1;
	}
	return bytes_append(dst, data, len);
}

static int shell_append_sed_range(byte_buf *out, const unsigned char *content, uint32_t len, int start, int end) {
	if (start <= 0 || end < start) {
		return 0;
	}
	int line = 1;
	uint32_t offset = 0;
	while (offset < len) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		uint32_t write_end = line_end;
		if (line_end < len && content[line_end] == '\n') {
			write_end = line_end + 1;
		}
		if (line >= start && line <= end) {
			if (!shell_eval_copy_bytes(out, content + offset, write_end - offset)) {
				return 0;
			}
		}
		if (line > end) {
			break;
		}
		line++;
		offset = write_end;
	}
	return 1;
}

static int shell_append_tail_lines(byte_buf *out, const unsigned char *content, uint32_t len, int count) {
	if (count < 0) {
		return 0;
	}
	int total = count_lines(content, len);
	int start = total - count + 1;
	if (start < 1) {
		start = 1;
	}
	return shell_append_sed_range(out, content, len, start, total);
}

static int shell_append_fixed_grep(byte_buf *out, const unsigned char *content, uint32_t len, const char *pattern, int quiet, int *matched) {
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
		if (mem_contains_bytes(content + offset, line_end - offset, (const unsigned char *)pattern, pattern_len)) {
			*matched = 1;
			if (quiet) {
				return 1;
			}
			if (!shell_eval_copy_bytes(out, content + offset, line_end - offset) ||
			    !bytes_append_byte(out, '\n')) {
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

static int shell_parse_line_count_arg(int argc, char argv[SHELL_IR_MAX_ARGS][SHELL_IR_WORD_BUF], int *count) {
	if (argc == 3 && strcmp(argv[1], "-n") == 0) {
		char *end = NULL;
		errno = 0;
		long parsed = strtol(argv[2], &end, 10);
		if (errno != 0 || end == argv[2] || *end != '\0' || parsed < 0 || parsed > 100000) {
			return 0;
		}
		*count = (int)parsed;
		return 1;
	}
	if (argc == 2 && strncmp(argv[1], "-n", 2) == 0 && argv[1][2] != '\0') {
		char *end = NULL;
		errno = 0;
		long parsed = strtol(argv[1] + 2, &end, 10);
		if (errno != 0 || end == argv[1] + 2 || *end != '\0' || parsed < 0 || parsed > 100000) {
			return 0;
		}
		*count = (int)parsed;
		return 1;
	}
	return 0;
}

static int shell_parse_stdin_grep(int argc, char argv[SHELL_IR_MAX_ARGS][SHELL_IR_WORD_BUF], const char **pattern, int *quiet) {
	*quiet = 0;
	*pattern = NULL;
	int fixed = 0;
	for (int i = 1; i < argc; i++) {
		if (strcmp(argv[i], "-q") == 0) {
			*quiet = 1;
			continue;
		}
		if (strcmp(argv[i], "-F") == 0) {
			fixed = 1;
			continue;
		}
		if (argv[i][0] == '-') {
			return 0;
		}
		if (*pattern != NULL) {
			return 0;
		}
		*pattern = argv[i];
	}
	return fixed && *pattern != NULL;
}

static int shell_eval_filter_exec(shell_ir_node *node, const byte_buf *input, shell_eval_result *out) {
	if (node == NULL || input == NULL || out == NULL || node->argc <= 0) {
		return 0;
	}
	const char *name = base_name(node->argv[0]);
	preload_trace("shell-ir-filter", name);
	if (strcmp(name, "cat") == 0 && node->argc == 1) {
		if (!shell_eval_append(&out->stdout_buf, input)) {
			return 0;
		}
		out->exit_code = 0;
		return 1;
	}
	int count = 0;
	if (strcmp(name, "head") == 0 && shell_parse_line_count_arg(node->argc, node->argv, &count)) {
		if (!shell_append_sed_range(&out->stdout_buf, input->data, (uint32_t)input->len, 1, count)) {
			return 0;
		}
		out->exit_code = 0;
		return 1;
	}
	if (strcmp(name, "tail") == 0 && shell_parse_line_count_arg(node->argc, node->argv, &count)) {
		if (!shell_append_tail_lines(&out->stdout_buf, input->data, (uint32_t)input->len, count)) {
			return 0;
		}
		out->exit_code = 0;
		return 1;
	}
	const char *pattern = NULL;
	int quiet = 0;
	if (strcmp(name, "grep") == 0 && shell_parse_stdin_grep(node->argc, node->argv, &pattern, &quiet)) {
		int matched = 0;
		if (!shell_append_fixed_grep(&out->stdout_buf, input->data, (uint32_t)input->len, pattern, quiet, &matched)) {
			return 0;
		}
		out->exit_code = matched ? 0 : 1;
		return 1;
	}
	return 0;
}

static int shell_prepare_warm_file_output(int argc, char **argv, shell_eval_result *out, long long replay_start_ns) {
	policy_invocation inv;
	if (!normalize_invocation(argc, argv, &inv)) {
		return 0;
	}
	if (!is_warm_file_candidate(&inv) || !warm_file_replay_enabled()) {
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
	char key[HASH_HEX], epoch[256], path[PATH_BUF];
	int sed_start = 0, sed_end = 0, line_count = 0;
	if (!warm_file_proof(&inv, key, epoch, path, &sed_start, &sed_end, &line_count)) {
		unmap_snapshot(&snap);
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
	if (!snapshot_find(&snap, command_hash, epoch, HOT_KIND_WARM_FILE, &content, &content_len, &err, &err_len, &exit_code, &native_wall_ms) ||
	    err_len != 0 || exit_code != 0) {
		unmap_snapshot(&snap);
		return 0;
	}
	int ok = 0;
	if (strcmp(inv.argv[0], "cat") == 0) {
		ok = shell_eval_copy_bytes(&out->stdout_buf, content, content_len);
		out->exit_code = 0;
	} else if (strcmp(inv.argv[0], "sed") == 0) {
		ok = shell_append_sed_range(&out->stdout_buf, content, content_len, sed_start, sed_end);
		out->exit_code = 0;
	} else if (strcmp(inv.argv[0], "head") == 0) {
		ok = shell_append_sed_range(&out->stdout_buf, content, content_len, 1, line_count);
		out->exit_code = 0;
	} else if (strcmp(inv.argv[0], "tail") == 0) {
		ok = shell_append_tail_lines(&out->stdout_buf, content, content_len, line_count);
		out->exit_code = 0;
	} else if (strcmp(inv.argv[0], "grep") == 0) {
		const char *pattern = NULL;
		const char *grep_path = NULL;
		int quiet = 0;
		int matched = 0;
		ok = parse_fixed_grep_args(&inv, &pattern, &grep_path, &quiet) &&
		     shell_append_fixed_grep(&out->stdout_buf, content, content_len, pattern, quiet, &matched);
		out->exit_code = matched ? 0 : 1;
	}
	if (ok) {
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
	}
	unmap_snapshot(&snap);
	return ok;
}

static int shell_eval_source_exec(shell_ir_node *node, char *const envp[], shell_eval_result *out, long long replay_start_ns) {
	if (node == NULL || out == NULL || node->argc <= 0) {
		return 0;
	}
	char *argv[SHELL_IR_MAX_ARGS];
	for (int i = 0; i < node->argc; i++) {
		argv[i] = node->argv[i];
	}
	argv[node->argc] = NULL;
	preload_trace("shell-ir-source", argv[0]);
	if (!preload_tool_candidate(argv[0], argv) || !parsed_shell_replay_env_compatible(node->argc, argv, envp)) {
		preload_trace("shell-ir-source-policy-miss", argv[0]);
		return 0;
	}
	prepared_exact_replay prepared;
	if (prepare_exact_replay(node->argc, argv, &prepared)) {
		preload_trace("shell-ir-source-exact", argv[0]);
		int ok = shell_eval_copy_bytes(&out->stdout_buf, prepared.stdout_data, prepared.stdout_len) &&
		         shell_eval_copy_bytes(&out->stderr_buf, prepared.stderr_data, prepared.stderr_len);
		out->exit_code = prepared.exit_code;
		if (ok) {
			record_hot_replay_event(prepared.store_root, (long long)prepared.native_wall_ms, replay_start_ns);
		}
		release_prepared_exact_replay(&prepared);
		return ok;
	}
	if (shell_prepare_warm_file_output(node->argc, argv, out, replay_start_ns)) {
		return 1;
	}
	return 0;
}

static int shell_eval_node(shell_ir_plan *plan, int idx, char *const envp[], const byte_buf *input, shell_eval_result *out, long long replay_start_ns);

static int shell_eval_binary(shell_ir_plan *plan, int left_idx, int right_idx, char *const envp[], const byte_buf *input, shell_eval_result *out, long long replay_start_ns, shell_node_kind kind) {
	shell_eval_result left = {0};
	if (!shell_eval_node(plan, left_idx, envp, input, &left, replay_start_ns)) {
		return 0;
	}
	if (kind == SHELL_NODE_AND && left.exit_code != 0) {
		*out = left;
		return 1;
	}
	if (kind == SHELL_NODE_PIPE) {
		shell_eval_result right = {0};
		if (!shell_eval_node(plan, right_idx, envp, &left.stdout_buf, &right, replay_start_ns)) {
			shell_eval_free(&left);
			return 0;
		}
		int ok = shell_eval_append(&out->stderr_buf, &left.stderr_buf) &&
		         shell_eval_append(&out->stderr_buf, &right.stderr_buf) &&
		         shell_eval_append(&out->stdout_buf, &right.stdout_buf);
		out->exit_code = right.exit_code;
		shell_eval_free(&left);
		shell_eval_free(&right);
		return ok;
	}
	shell_eval_result right = {0};
	if (!shell_eval_node(plan, right_idx, envp, input, &right, replay_start_ns)) {
		shell_eval_free(&left);
		return 0;
	}
	int ok = shell_eval_append(&out->stdout_buf, &left.stdout_buf) &&
	         shell_eval_append(&out->stderr_buf, &left.stderr_buf) &&
	         shell_eval_append(&out->stdout_buf, &right.stdout_buf) &&
	         shell_eval_append(&out->stderr_buf, &right.stderr_buf);
	out->exit_code = right.exit_code;
	shell_eval_free(&left);
	shell_eval_free(&right);
	return ok;
}

static int shell_eval_node(shell_ir_plan *plan, int idx, char *const envp[], const byte_buf *input, shell_eval_result *out, long long replay_start_ns) {
	if (plan == NULL || out == NULL || idx < 0 || idx >= plan->count) {
		return 0;
	}
	shell_ir_node *node = &plan->nodes[idx];
	switch (node->kind) {
	case SHELL_NODE_EXEC:
		if (input != NULL) {
			return shell_eval_filter_exec(node, input, out);
		}
		return shell_eval_source_exec(node, envp, out, replay_start_ns);
	case SHELL_NODE_PIPE:
	case SHELL_NODE_AND:
	case SHELL_NODE_SEQ:
		return shell_eval_binary(plan, node->left, node->right, envp, input, out, replay_start_ns, node->kind);
	case SHELL_NODE_REDIR_NULL:
		if (!shell_eval_node(plan, node->left, envp, input, out, replay_start_ns)) {
			return 0;
		}
		if (node->fd == 1) {
			bytes_free(&out->stdout_buf);
			memset(&out->stdout_buf, 0, sizeof(out->stdout_buf));
			return 1;
		}
		if (node->fd == 2) {
			bytes_free(&out->stderr_buf);
			memset(&out->stderr_buf, 0, sizeof(out->stderr_buf));
			return 1;
		}
		return 0;
	default:
		return 0;
	}
}

static int shell_ir_evaluate_command(const char *command, char *const envp[], shell_eval_result *out, long long replay_start_ns) {
	shell_ir_plan *plan = (shell_ir_plan *)calloc(1, sizeof(*plan));
	if (plan == NULL) {
		return 0;
	}
	if (!shell_ir_parse(command, plan)) {
		preload_trace("shell-ir-parse-miss", NULL);
		free(plan);
		return 0;
	}
	preload_trace("shell-ir-parse-ok", NULL);
	int ok = shell_eval_node(plan, plan->root, envp, NULL, out, replay_start_ns);
	free(plan);
	return ok;
}

static int shell_eval_emit_and_exit(shell_eval_result *res) {
	if (res == NULL) {
		return 0;
	}
	if (res->stdout_buf.len > MAX_FAST_OUTPUT_BYTES || res->stderr_buf.len > MAX_FAST_OUTPUT_BYTES) {
		return 0;
	}
	if (res->stdout_buf.len > 0 && !write_all(STDOUT_FILENO, res->stdout_buf.data, res->stdout_buf.len)) {
		return 0;
	}
	if (res->stderr_buf.len > 0 && !write_all(STDERR_FILENO, res->stderr_buf.data, res->stderr_buf.len)) {
		return 0;
	}
	int exit_code = res->exit_code;
	shell_eval_free(res);
	_exit(exit_code);
}

static int maybe_replay_exec(const char *path, char *const argv[], char *const envp[]) {
	if (squire_preload_guard || !preload_active()) {
		return 0;
	}
	if (envp != NULL && envp != environ) {
		return 0;
	}
	if (!preload_tool_candidate(path, argv)) {
		return 0;
	}
	int argc = count_argv(argv);
	if (argc <= 0) {
		return 0;
	}
	squire_preload_guard = 1;
	int replayed = try_replay(argc, (char **)argv);
	if (!replayed && require_hit()) {
		fprintf(stderr, "squire preload: hot snapshot miss\n");
		_exit(91);
	}
	squire_preload_guard = 0;
	return replayed;
}

static const char *envp_value(char *const envp[], const char *key) {
	if (envp == NULL || key == NULL) {
		return NULL;
	}
	size_t key_len = strlen(key);
	for (size_t i = 0; envp[i] != NULL; i++) {
		if (strncmp(envp[i], key, key_len) == 0 && envp[i][key_len] == '=') {
			return envp[i] + key_len + 1;
		}
	}
	return NULL;
}

static int env_values_match(char *const envp[], const char *key) {
	const char *expected = getenv(key);
	const char *actual = envp_value(envp, key);
	if (expected == NULL || expected[0] == '\0') {
		return actual == NULL || actual[0] == '\0';
	}
	return actual != NULL && strcmp(actual, expected) == 0;
}

static int envp_shell_replay_compatible(char *const envp[]) {
	if (envp == NULL || envp == environ) {
		return 1;
	}
	if (!env_values_match(envp, "PATH") ||
	    !env_values_match(envp, "HOME") ||
	    !env_values_match(envp, "LANG") ||
	    !env_values_match(envp, "SQUIRE_STORE_ROOT") ||
	    !env_values_match(envp, "SQUIRE_KERNEL_STORE_ROOT") ||
	    !env_values_match(envp, "SQUIRE_SHIM_REAL_PATH") ||
	    !env_values_match(envp, "SQUIRE_REAL_GIT") ||
	    !env_values_match(envp, "SQUIRE_REAL_CAT") ||
	    !env_values_match(envp, "SQUIRE_REAL_SED") ||
	    !env_values_match(envp, "SQUIRE_REAL_HEAD") ||
	    !env_values_match(envp, "SQUIRE_REAL_TAIL") ||
	    !env_values_match(envp, "SQUIRE_REAL_LS") ||
	    !env_values_match(envp, "SQUIRE_REAL_FILE") ||
	    !env_values_match(envp, "SQUIRE_REAL_GREP") ||
	    !env_values_match(envp, "SQUIRE_REAL_PRINTENV") ||
	    !env_values_match(envp, "SQUIRE_REAL_WHOAMI") ||
	    !env_values_match(envp, "SQUIRE_REAL_UNAME") ||
	    !env_values_match(envp, "SQUIRE_REAL_ID") ||
	    !env_values_match(envp, "SQUIRE_REAL_HOSTNAME") ||
	    !env_values_match(envp, "SQUIRE_REAL_RG")) {
		return 0;
	}
	for (size_t i = 0; envp[i] != NULL; i++) {
		if (env_has_prefix(envp[i], "GIT_") || env_has_prefix(envp[i], "LC_")) {
			char key[128];
			const char *eq = strchr(envp[i], '=');
			size_t key_len = eq == NULL ? strlen(envp[i]) : (size_t)(eq - envp[i]);
			if (key_len >= sizeof(key)) {
				return 0;
			}
			memcpy(key, envp[i], key_len);
			key[key_len] = '\0';
			if (!env_values_match(envp, key)) {
				return 0;
			}
		}
	}
	return 1;
}

static int parsed_git_metadata_command(int argc, char *const argv[]) {
	if (argv == NULL || argc < 3 || strcmp(argv[0], "git") != 0 || strcmp(argv[1], "rev-parse") != 0) {
		return 0;
	}
	if (argc == 3) {
		return strcmp(argv[2], "HEAD") == 0 ||
		       strcmp(argv[2], "--git-dir") == 0 ||
		       strcmp(argv[2], "--show-toplevel") == 0 ||
		       strcmp(argv[2], "--is-inside-work-tree") == 0;
	}
	return argc == 4 &&
	       strcmp(argv[2], "--abbrev-ref") == 0 &&
	       strcmp(argv[3], "HEAD") == 0;
}

static int git_metadata_env_key_affects_output(const char *key) {
	if (key == NULL) {
		return 0;
	}
	return strcmp(key, "GIT_DIR") == 0 ||
	       strcmp(key, "GIT_WORK_TREE") == 0 ||
	       strcmp(key, "GIT_COMMON_DIR") == 0 ||
	       strcmp(key, "GIT_NAMESPACE") == 0 ||
	       strcmp(key, "GIT_INDEX_FILE") == 0 ||
	       strcmp(key, "GIT_CONFIG") == 0 ||
	       strcmp(key, "GIT_CONFIG_GLOBAL") == 0 ||
	       strcmp(key, "GIT_CONFIG_SYSTEM") == 0 ||
	       strcmp(key, "GIT_CONFIG_NOSYSTEM") == 0 ||
	       strcmp(key, "GIT_CONFIG_COUNT") == 0 ||
	       strcmp(key, "GIT_CONFIG_PARAMETERS") == 0 ||
	       strncmp(key, "GIT_CONFIG_KEY_", strlen("GIT_CONFIG_KEY_")) == 0 ||
	       strncmp(key, "GIT_CONFIG_VALUE_", strlen("GIT_CONFIG_VALUE_")) == 0 ||
	       strcmp(key, "GIT_CEILING_DIRECTORIES") == 0 ||
	       strcmp(key, "GIT_DISCOVERY_ACROSS_FILESYSTEM") == 0 ||
	       strcmp(key, "GIT_OBJECT_DIRECTORY") == 0 ||
	       strcmp(key, "GIT_ALTERNATE_OBJECT_DIRECTORIES") == 0 ||
	       strcmp(key, "LANG") == 0 ||
	       strcmp(key, "LANGUAGE") == 0 ||
	       strncmp(key, "LC_", strlen("LC_")) == 0;
}

static int envp_danger_keys_match(char *const envp[], char *const source[]) {
	if (source == NULL) {
		return 1;
	}
	for (size_t i = 0; source[i] != NULL; i++) {
		const char *eq = strchr(source[i], '=');
		size_t key_len = eq == NULL ? strlen(source[i]) : (size_t)(eq - source[i]);
		if (key_len == 0 || key_len >= 128) {
			continue;
		}
		char key[128];
		memcpy(key, source[i], key_len);
		key[key_len] = '\0';
		if (git_metadata_env_key_affects_output(key) && !env_values_match(envp, key)) {
			return 0;
		}
	}
	return 1;
}

static int envp_git_metadata_replay_compatible(char *const envp[]) {
	if (envp == NULL || envp == environ) {
		return 1;
	}
	return envp_danger_keys_match(envp, envp) && envp_danger_keys_match(envp, environ);
}

static int parsed_shell_replay_env_compatible(int argc, char *const argv[], char *const envp[]) {
	if (parsed_git_metadata_command(argc, argv)) {
		return envp_git_metadata_replay_compatible(envp);
	}
	if (argc == 2 && argv != NULL && strcmp(argv[0], "printenv") == 0) {
		return env_values_match(envp, argv[1]) && envp_shell_replay_compatible(envp);
	}
	return envp_shell_replay_compatible(envp);
}

static int maybe_replay_shell_execve(const char *path, char *const argv[], char *const envp[]) {
	if (squire_preload_trace_enabled && path != NULL && strstr(path, "sh") != NULL) {
		preload_trace_shell_argv("shell-execve-entry", path, argv);
	}
	if (squire_preload_guard) {
		preload_trace("shell-execve-skip-guard", path);
		return 0;
	}
	if (!preload_active()) {
		preload_trace("shell-execve-skip-disabled", path);
		return 0;
	}
	if (!is_shell_launcher_name(path, argv)) {
		if (squire_preload_trace_enabled && path != NULL && strstr(path, "sh") != NULL) {
			preload_trace_shell_argv("shell-execve-skip-name", path, argv);
		}
		return 0;
	}
	const char *command = shell_command_arg(argv);
	if (command == NULL) {
		preload_trace_shell_argv("shell-execve-skip-argv", path, argv);
		return 0;
	}
	if (shell_ir_enabled()) {
		shell_eval_result shell_res = {0};
		preload_trace("shell-execve-ir-attempt", path);
		if (shell_ir_evaluate_command(command, envp, &shell_res, now_monotonic_ns())) {
			preload_trace("shell-execve-ir-replay", path);
			shell_eval_emit_and_exit(&shell_res);
		}
		shell_eval_free(&shell_res);
	}
	int parsed_argc = 0;
	char *parsed_argv[MAX_ARGC];
	char parsed_storage[MAX_ARGC][PATH_BUF];
	if (!parse_simple_shell_command(command, &parsed_argc, parsed_argv, parsed_storage)) {
		preload_trace("shell-execve-skip-parse", path);
		return 0;
	}
	if (!preload_tool_candidate(parsed_argv[0], parsed_argv)) {
		preload_trace("shell-execve-skip-policy", parsed_argv[0]);
		return 0;
	}
	if (!parsed_shell_replay_env_compatible(parsed_argc, parsed_argv, envp)) {
		preload_trace("shell-execve-skip-env", parsed_argv[0]);
		if (require_hit()) {
			fprintf(stderr, "squire preload: shell replay env incompatible\n");
			_exit(91);
		}
		return 0;
	}
	preload_trace("shell-execve-attempt", parsed_argv[0]);
	squire_preload_guard = 1;
	int replayed = try_replay(parsed_argc, parsed_argv);
	if (!replayed && require_hit()) {
		fprintf(stderr, "squire preload: shell hot snapshot miss\n");
		_exit(91);
	}
	squire_preload_guard = 0;
	if (replayed) {
		preload_trace("shell-execve-replay", parsed_argv[0]);
	} else {
		preload_trace("shell-execve-miss", parsed_argv[0]);
	}
	return replayed;
}

static int env_has_prefix(const char *entry, const char *prefix) {
	return entry != NULL && strncmp(entry, prefix, strlen(prefix)) == 0;
}

static char **scrub_preload_envp_in_place(char *const envp[]) {
	if (envp == NULL) {
		return NULL;
	}
	char **mutable_envp = (char **)envp;
	size_t out = 0;
	for (size_t i = 0; mutable_envp[i] != NULL; i++) {
		char *entry = mutable_envp[i];
		if (env_has_prefix(entry, "SQUIRE_PRELOAD_ENABLE=") ||
		    env_has_prefix(entry, "SQUIRE_PRELOAD_LIB=") ||
		    env_has_prefix(entry, "SQUIRE_SHIM_REQUIRE_HIT=") ||
		    env_has_prefix(entry, "DYLD_INSERT_LIBRARIES=") ||
		    env_has_prefix(entry, "LD_PRELOAD=")) {
			continue;
		}
		mutable_envp[out++] = entry;
	}
	mutable_envp[out] = NULL;
	return mutable_envp;
}

static char *strip_preload_path_value(const char *key, const char *entry) {
	const char *lib = getenv("SQUIRE_PRELOAD_LIB");
	if (lib == NULL || lib[0] == '\0' || !env_has_prefix(entry, key)) {
		return NULL;
	}
	const char *value = entry + strlen(key);
	size_t lib_len = strlen(lib);
	byte_buf out = {0};
	const char *cursor = value;
	while (*cursor != '\0') {
		const char *end = strchr(cursor, ':');
		size_t part_len = end == NULL ? strlen(cursor) : (size_t)(end - cursor);
		if (!(part_len == lib_len && strncmp(cursor, lib, lib_len) == 0)) {
			if (out.len > 0 && !bytes_append_byte(&out, ':')) {
				free(out.data);
				return NULL;
			}
			if (!bytes_append(&out, cursor, part_len)) {
				free(out.data);
				return NULL;
			}
		}
		if (end == NULL) {
			break;
		}
		cursor = end + 1;
	}
	if (out.len == 0) {
		free(out.data);
		return strdup("");
	}
	if (!bytes_append_byte(&out, '\0')) {
		free(out.data);
		return NULL;
	}
	return (char *)out.data;
}

static char **scrub_preload_envp(char *const envp[]) {
	if (envp == NULL) {
		return NULL;
	}
	size_t count = 0;
	while (envp[count] != NULL) {
		count++;
	}
	char **copy = (char **)calloc(count + 1, sizeof(char *));
	if (copy == NULL) {
		return (char **)envp;
	}
	size_t out = 0;
	for (size_t i = 0; i < count; i++) {
		char *entry = envp[i];
		if (env_has_prefix(entry, "SQUIRE_PRELOAD_ENABLE=") ||
		    env_has_prefix(entry, "SQUIRE_PRELOAD_LIB=") ||
		    env_has_prefix(entry, "SQUIRE_SHIM_REQUIRE_HIT=")) {
			continue;
		}
		if (env_has_prefix(entry, "DYLD_INSERT_LIBRARIES=")) {
			char *stripped = strip_preload_path_value("DYLD_INSERT_LIBRARIES=", entry);
			if (stripped != NULL) {
				if (stripped[0] == '\0') {
					free(stripped);
					continue;
				}
				size_t len = strlen("DYLD_INSERT_LIBRARIES=") + strlen(stripped) + 1;
				char *rebuilt = (char *)malloc(len);
				if (rebuilt != NULL) {
					snprintf(rebuilt, len, "DYLD_INSERT_LIBRARIES=%s", stripped);
					copy[out++] = rebuilt;
					free(stripped);
					continue;
				}
				free(stripped);
			}
		}
		if (env_has_prefix(entry, "LD_PRELOAD=")) {
			char *stripped = strip_preload_path_value("LD_PRELOAD=", entry);
			if (stripped != NULL) {
				if (stripped[0] == '\0') {
					free(stripped);
					continue;
				}
				size_t len = strlen("LD_PRELOAD=") + strlen(stripped) + 1;
				char *rebuilt = (char *)malloc(len);
				if (rebuilt != NULL) {
					snprintf(rebuilt, len, "LD_PRELOAD=%s", stripped);
					copy[out++] = rebuilt;
					free(stripped);
					continue;
				}
				free(stripped);
			}
		}
		copy[out++] = entry;
	}
	copy[out] = NULL;
	return copy;
}

static char **scrub_preload_envp_for_helper(char *const envp[]) {
	if (envp == NULL) {
		return NULL;
	}
	size_t count = 0;
	while (envp[count] != NULL) {
		count++;
	}
	char **copy = (char **)calloc(count + 1, sizeof(char *));
	if (copy == NULL) {
		return (char **)envp;
	}
	size_t out = 0;
	for (size_t i = 0; i < count; i++) {
		char *entry = envp[i];
		if (env_has_prefix(entry, "SQUIRE_PRELOAD_ENABLE=") ||
		    env_has_prefix(entry, "SQUIRE_PRELOAD_LIB=") ||
		    env_has_prefix(entry, "DYLD_INSERT_LIBRARIES=") ||
		    env_has_prefix(entry, "LD_PRELOAD=")) {
			continue;
		}
		copy[out++] = entry;
	}
	copy[out] = NULL;
	return copy;
}

typedef int (*execve_fn)(const char *, char *const[], char *const[]);
typedef int (*execv_fn)(const char *, char *const[]);
typedef int (*execvp_fn)(const char *, char *const[]);
typedef int (*posix_spawn_fn)(pid_t *, const char *, const posix_spawn_file_actions_t *, const posix_spawnattr_t *, char *const[], char *const[]);
typedef int (*posix_spawnp_fn)(pid_t *, const char *, const posix_spawn_file_actions_t *, const posix_spawnattr_t *, char *const[], char *const[]);
typedef int (*file_actions_init_fn)(posix_spawn_file_actions_t *);
typedef int (*file_actions_destroy_fn)(posix_spawn_file_actions_t *);
typedef int (*file_actions_addclose_fn)(posix_spawn_file_actions_t *, int);
typedef int (*file_actions_adddup2_fn)(posix_spawn_file_actions_t *, int, int);
typedef pid_t (*waitpid_fn)(pid_t, int *, int);
typedef pid_t (*wait_fn)(int *);

int squire_preload_file_actions_init(posix_spawn_file_actions_t *actions);
int squire_preload_file_actions_destroy(posix_spawn_file_actions_t *actions);
int squire_preload_file_actions_addclose(posix_spawn_file_actions_t *actions, int fd);
int squire_preload_file_actions_adddup2(posix_spawn_file_actions_t *actions, int fd, int newfd);

static execve_fn real_execve_ptr;
static execv_fn real_execv_ptr;
static execvp_fn real_execvp_ptr;
static posix_spawn_fn real_posix_spawn_ptr;
static posix_spawnp_fn real_posix_spawnp_ptr;
static posix_spawn_fn kernel_posix_spawn_ptr;
static file_actions_init_fn real_file_actions_init_ptr;
static file_actions_destroy_fn real_file_actions_destroy_ptr;
static file_actions_addclose_fn real_file_actions_addclose_ptr;
static file_actions_adddup2_fn real_file_actions_adddup2_ptr;
static waitpid_fn real_waitpid_ptr;
static wait_fn real_wait_ptr;

#if defined(__APPLE__) && defined(__clang__)
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
#endif
static int native_execve_call(const char *path, char *const argv[], char *const envp[]) {
#if defined(SYS_execve)
	return (int)syscall(SYS_execve, path, argv, envp);
#else
	if (real_execve_ptr == NULL) {
		errno = ENOSYS;
		return -1;
	}
	return real_execve_ptr(path, argv, envp);
#endif
}
#if defined(__APPLE__) && defined(__clang__)
#pragma clang diagnostic pop
#endif

static int native_posix_spawn_call(pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
#if defined(__APPLE__)
	if (kernel_posix_spawn_ptr == NULL) {
		return ENOSYS;
	}
	int rc = kernel_posix_spawn_ptr(pid, path, file_actions, attrp, argv, envp);
	return rc == -1 ? errno : rc;
#else
	if (real_posix_spawn_ptr == NULL) {
		return ENOSYS;
	}
	return real_posix_spawn_ptr(pid, path, file_actions, attrp, argv, envp);
#endif
}

static pid_t native_waitpid_call(pid_t pid, int *status, int options) {
#if defined(__APPLE__) && defined(SYS_wait4)
#if defined(__clang__)
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
#endif
	return (pid_t)syscall(SYS_wait4, pid, status, options, NULL);
#if defined(__clang__)
#pragma clang diagnostic pop
#endif
#else
	if (real_waitpid_ptr == NULL) {
		errno = ECHILD;
		return -1;
	}
	return real_waitpid_ptr(pid, status, options);
#endif
}

#define MAX_TRACKED_FILE_ACTIONS 128
#define MAX_FILE_ACTIONS_PER_RECORD 32
#define FILE_ACTION_CLOSE 1
#define FILE_ACTION_DUP2 2

typedef struct {
	int kind;
	int fd;
	int newfd;
} tracked_file_action;

typedef struct {
	const posix_spawn_file_actions_t *key;
	int used;
	int unsupported;
	int count;
	tracked_file_action actions[MAX_FILE_ACTIONS_PER_RECORD];
} tracked_file_actions_record;

static tracked_file_actions_record tracked_file_actions[MAX_TRACKED_FILE_ACTIONS];

#define MAX_SYNTHETIC_CHILDREN 128
#define SYNTHETIC_PID_START 1073000000
#define SYNTHETIC_PID_FLOOR 1070000000

typedef struct {
	int used;
	pid_t pid;
	int exit_code;
} synthetic_child_record;

static synthetic_child_record synthetic_children[MAX_SYNTHETIC_CHILDREN];
static volatile int synthetic_child_lock;
static pid_t next_synthetic_pid = SYNTHETIC_PID_START;

#if defined(__APPLE__)
#if defined(__clang__)
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
#endif
static void *darwin_lookup_kernel_symbol(const char *symbol) {
	const struct mach_header *image = NSAddImage("/usr/lib/system/libsystem_kernel.dylib", NSADDIMAGE_OPTION_RETURN_ON_ERROR);
	if (image == NULL) {
		return NULL;
	}
	NSSymbol sym = NSLookupSymbolInImage(image, symbol, NSLOOKUPSYMBOLINIMAGE_OPTION_BIND | NSLOOKUPSYMBOLINIMAGE_OPTION_RETURN_ON_ERROR);
	if (sym == NULL) {
		return NULL;
	}
	return NSAddressOfSymbol(sym);
}
#if defined(__clang__)
#pragma clang diagnostic pop
#endif
#endif

static void synthetic_lock(void) {
	while (__sync_lock_test_and_set(&synthetic_child_lock, 1)) {
	}
}

static void synthetic_unlock(void) {
	__sync_lock_release(&synthetic_child_lock);
}

static int register_synthetic_child(int exit_code, pid_t *pid_out) {
	if (pid_out == NULL) {
		return 0;
	}
	synthetic_lock();
	for (int i = 0; i < MAX_SYNTHETIC_CHILDREN; i++) {
		if (!synthetic_children[i].used) {
			pid_t pid = next_synthetic_pid--;
			if (next_synthetic_pid < SYNTHETIC_PID_FLOOR) {
				next_synthetic_pid = SYNTHETIC_PID_START;
			}
			synthetic_children[i].used = 1;
			synthetic_children[i].pid = pid;
			synthetic_children[i].exit_code = exit_code;
			*pid_out = pid;
			synthetic_unlock();
			return 1;
		}
	}
	synthetic_unlock();
	return 0;
}

static pid_t consume_synthetic_child(pid_t pid, int *status) {
	int found = -1;
	synthetic_lock();
	if (pid > 0) {
		for (int i = 0; i < MAX_SYNTHETIC_CHILDREN; i++) {
			if (synthetic_children[i].used && synthetic_children[i].pid == pid) {
				found = i;
				break;
			}
		}
	} else if (pid == -1) {
		for (int i = 0; i < MAX_SYNTHETIC_CHILDREN; i++) {
			if (synthetic_children[i].used) {
				found = i;
				break;
			}
		}
	}
	if (found < 0) {
		synthetic_unlock();
		return 0;
	}
	pid_t out_pid = synthetic_children[found].pid;
	int exit_code = synthetic_children[found].exit_code & 0xff;
	synthetic_children[found].used = 0;
	synthetic_children[found].pid = 0;
	synthetic_children[found].exit_code = 0;
	synthetic_unlock();
	if (status != NULL) {
		*status = exit_code << 8;
	}
	return out_pid;
}

static void discard_synthetic_child(pid_t pid) {
	if (pid <= 0) {
		return;
	}
	synthetic_lock();
	for (int i = 0; i < MAX_SYNTHETIC_CHILDREN; i++) {
		if (synthetic_children[i].used && synthetic_children[i].pid == pid) {
			synthetic_children[i].used = 0;
			synthetic_children[i].pid = 0;
			synthetic_children[i].exit_code = 0;
			break;
		}
	}
	synthetic_unlock();
}

__attribute__((constructor)) static void squire_preload_init(void) {
	squire_preload_trace_enabled = env_truthy("SQUIRE_PRELOAD_TRACE");
	squire_preload_active = env_truthy("SQUIRE_PRELOAD_ENABLE") ||
	                        getenv("SQUIRE_PRELOAD_LIB") != NULL ||
	                        env_value_contains("LD_PRELOAD", "squire-preload") ||
	                        env_value_contains("DYLD_INSERT_LIBRARIES", "squire-preload");
	squire_preload_require_hit = env_truthy("SQUIRE_SHIM_REQUIRE_HIT");
	preload_trace("init-shell-v3", NULL);
	real_execve_ptr = (execve_fn)dlsym(RTLD_NEXT, "execve");
	real_execv_ptr = (execv_fn)dlsym(RTLD_NEXT, "execv");
	real_execvp_ptr = (execvp_fn)dlsym(RTLD_NEXT, "execvp");
	real_posix_spawn_ptr = (posix_spawn_fn)dlsym(RTLD_NEXT, "posix_spawn");
	real_posix_spawnp_ptr = (posix_spawnp_fn)dlsym(RTLD_NEXT, "posix_spawnp");
	real_file_actions_init_ptr = (file_actions_init_fn)dlsym(RTLD_NEXT, "posix_spawn_file_actions_init");
	real_file_actions_destroy_ptr = (file_actions_destroy_fn)dlsym(RTLD_NEXT, "posix_spawn_file_actions_destroy");
	real_file_actions_addclose_ptr = (file_actions_addclose_fn)dlsym(RTLD_NEXT, "posix_spawn_file_actions_addclose");
	real_file_actions_adddup2_ptr = (file_actions_adddup2_fn)dlsym(RTLD_NEXT, "posix_spawn_file_actions_adddup2");
	real_waitpid_ptr = (waitpid_fn)dlsym(RTLD_NEXT, "waitpid");
	real_wait_ptr = (wait_fn)dlsym(RTLD_NEXT, "wait");
#if defined(__APPLE__)
	kernel_posix_spawn_ptr = (posix_spawn_fn)darwin_lookup_kernel_symbol("_posix_spawn");
	if (real_file_actions_init_ptr == NULL || real_file_actions_init_ptr == squire_preload_file_actions_init) {
		real_file_actions_init_ptr = (file_actions_init_fn)darwin_lookup_kernel_symbol("_posix_spawn_file_actions_init");
	}
	if (real_file_actions_destroy_ptr == NULL || real_file_actions_destroy_ptr == squire_preload_file_actions_destroy) {
		real_file_actions_destroy_ptr = (file_actions_destroy_fn)darwin_lookup_kernel_symbol("_posix_spawn_file_actions_destroy");
	}
	if (real_file_actions_addclose_ptr == NULL || real_file_actions_addclose_ptr == squire_preload_file_actions_addclose) {
		real_file_actions_addclose_ptr = (file_actions_addclose_fn)darwin_lookup_kernel_symbol("_posix_spawn_file_actions_addclose");
	}
	if (real_file_actions_adddup2_ptr == NULL || real_file_actions_adddup2_ptr == squire_preload_file_actions_adddup2) {
		real_file_actions_adddup2_ptr = (file_actions_adddup2_fn)darwin_lookup_kernel_symbol("_posix_spawn_file_actions_adddup2");
	}
#endif
}

static tracked_file_actions_record *find_file_actions_record(const posix_spawn_file_actions_t *key) {
	if (key == NULL) {
		return NULL;
	}
	for (int i = 0; i < MAX_TRACKED_FILE_ACTIONS; i++) {
		if (tracked_file_actions[i].used && tracked_file_actions[i].key == key) {
			return &tracked_file_actions[i];
		}
	}
	return NULL;
}

static tracked_file_actions_record *ensure_file_actions_record(const posix_spawn_file_actions_t *key) {
	tracked_file_actions_record *existing = find_file_actions_record(key);
	if (existing != NULL) {
		return existing;
	}
	for (int i = 0; i < MAX_TRACKED_FILE_ACTIONS; i++) {
		if (!tracked_file_actions[i].used) {
			memset(&tracked_file_actions[i], 0, sizeof(tracked_file_actions[i]));
			tracked_file_actions[i].used = 1;
			tracked_file_actions[i].key = key;
			return &tracked_file_actions[i];
		}
	}
	return NULL;
}

static void remove_file_actions_record(const posix_spawn_file_actions_t *key) {
	tracked_file_actions_record *record = find_file_actions_record(key);
	if (record != NULL) {
		memset(record, 0, sizeof(*record));
	}
}

static void append_file_action(const posix_spawn_file_actions_t *key, int kind, int fd, int newfd) {
	tracked_file_actions_record *record = ensure_file_actions_record(key);
	if (record == NULL) {
		return;
	}
	if (record->count >= MAX_FILE_ACTIONS_PER_RECORD) {
		record->unsupported = 1;
		return;
	}
	record->actions[record->count].kind = kind;
	record->actions[record->count].fd = fd;
	record->actions[record->count].newfd = newfd;
	record->count++;
}

static int apply_tracked_file_actions(const posix_spawn_file_actions_t *key) {
	tracked_file_actions_record *record = find_file_actions_record(key);
	if (record == NULL || record->unsupported) {
		return 0;
	}
	for (int i = 0; i < record->count; i++) {
		tracked_file_action *action = &record->actions[i];
		if (action->kind == FILE_ACTION_CLOSE) {
			if (close(action->fd) != 0 && errno != EBADF) {
				return 0;
			}
			continue;
		}
		if (action->kind == FILE_ACTION_DUP2) {
			if (dup2(action->fd, action->newfd) < 0) {
				return 0;
			}
			continue;
		}
		return 0;
	}
	return 1;
}

static int fd_was_closed(int fd, const int *closed_fds, int closed_count) {
	for (int i = 0; i < closed_count; i++) {
		if (closed_fds[i] == fd) {
			return 1;
		}
	}
	return 0;
}

static int resolve_synthetic_output_fds(const posix_spawn_file_actions_t *key, int *stdout_fd, int *stderr_fd) {
	if (stdout_fd == NULL || stderr_fd == NULL) {
		return 0;
	}
	int out_fd = STDOUT_FILENO;
	int err_fd = STDERR_FILENO;
	int out_open = 1;
	int err_open = 1;
	int closed_fds[MAX_FILE_ACTIONS_PER_RECORD];
	int closed_count = 0;
	tracked_file_actions_record *record = find_file_actions_record(key);
	if (key != NULL && (record == NULL || record->unsupported)) {
		return 0;
	}
	for (int i = 0; record != NULL && i < record->count; i++) {
		tracked_file_action *action = &record->actions[i];
		if (action->kind == FILE_ACTION_CLOSE) {
			if (closed_count < MAX_FILE_ACTIONS_PER_RECORD) {
				closed_fds[closed_count++] = action->fd;
			}
			if (action->fd == STDOUT_FILENO) {
				out_open = 0;
			}
			if (action->fd == STDERR_FILENO) {
				err_open = 0;
			}
			continue;
		}
		if (action->kind == FILE_ACTION_DUP2) {
			if (fd_was_closed(action->fd, closed_fds, closed_count)) {
				return 0;
			}
			if (action->newfd == STDOUT_FILENO) {
				out_fd = action->fd;
				out_open = 1;
			}
			if (action->newfd == STDERR_FILENO) {
				err_fd = action->fd;
				err_open = 1;
			}
			continue;
		}
		return 0;
	}
	if (!out_open || !err_open || out_fd < 0 || err_fd < 0) {
		return 0;
	}
	*stdout_fd = out_fd;
	*stderr_fd = err_fd;
	return 1;
}

static int synthetic_stdout_fd_supported(int fd, uint32_t len) {
	if (fd < 0) {
		return 0;
	}
#if defined(PIPE_BUF)
	if (len > PIPE_BUF) {
		return 0;
	}
#else
	if (len > 512) {
		return 0;
	}
#endif
	struct stat st;
	if (fstat(fd, &st) != 0) {
		return 0;
	}
	return S_ISFIFO(st.st_mode);
}

static int emit_synthetic_prepared_replay(prepared_exact_replay *prepared, const posix_spawn_file_actions_t *file_actions, pid_t *pid_out) {
	if (prepared == NULL || pid_out == NULL || !prepared->synthetic_safe) {
		return 0;
	}
	if (prepared->exit_code != 0 || prepared->stderr_len != 0) {
		return 0;
	}
	int stdout_fd, stderr_fd;
	if (!resolve_synthetic_output_fds(file_actions, &stdout_fd, &stderr_fd)) {
		return 0;
	}
	if (!synthetic_stdout_fd_supported(stdout_fd, prepared->stdout_len)) {
		return 0;
	}
	pid_t synthetic_pid;
	if (!register_synthetic_child(prepared->exit_code, &synthetic_pid)) {
		return 0;
	}
	ssize_t n;
	do {
		n = write(stdout_fd, prepared->stdout_data, prepared->stdout_len);
	} while (n < 0 && errno == EINTR);
	if (n != (ssize_t)prepared->stdout_len) {
		discard_synthetic_child(synthetic_pid);
		return 0;
	}
	record_hot_replay_event_kind(prepared->store_root, HOT_CLIENT_PROOF_C_SYNTHETIC, (long long)prepared->native_wall_ms, prepared->replay_start_ns);
	*pid_out = synthetic_pid;
	(void)stderr_fd;
	return 1;
}

static int emit_synthetic_buffer_replay(const char *store_root, uint64_t native_wall_ms, long long replay_start_ns, const byte_buf *stdout_buf, int exit_code, const posix_spawn_file_actions_t *file_actions, pid_t *pid_out) {
	if (store_root == NULL || stdout_buf == NULL || pid_out == NULL || exit_code != 0) {
		return 0;
	}
	if (stdout_buf->len > MAX_FAST_OUTPUT_BYTES) {
		return 0;
	}
	int stdout_fd, stderr_fd;
	if (!resolve_synthetic_output_fds(file_actions, &stdout_fd, &stderr_fd)) {
		return 0;
	}
	if (!synthetic_stdout_fd_supported(stdout_fd, (uint32_t)stdout_buf->len)) {
		return 0;
	}
	pid_t synthetic_pid;
	if (!register_synthetic_child(exit_code, &synthetic_pid)) {
		return 0;
	}
	if (stdout_buf->len > 0) {
		ssize_t n;
		do {
			n = write(stdout_fd, stdout_buf->data, stdout_buf->len);
		} while (n < 0 && errno == EINTR);
		if (n != (ssize_t)stdout_buf->len) {
			discard_synthetic_child(synthetic_pid);
			return 0;
		}
	}
	record_hot_replay_event_kind(store_root, HOT_CLIENT_PROOF_C_SYNTHETIC, (long long)native_wall_ms, replay_start_ns);
	*pid_out = synthetic_pid;
	(void)stderr_fd;
	return 1;
}

static int emit_synthetic_warm_file_replay(int argc, char **argv, const posix_spawn_file_actions_t *file_actions, pid_t *pid_out) {
	policy_invocation inv;
	if (!normalize_invocation(argc, argv, &inv) || !is_warm_file_candidate(&inv) || !warm_file_replay_enabled()) {
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
	char key[HASH_HEX], epoch[256], path[PATH_BUF];
	int sed_start = 0, sed_end = 0, line_count = 0;
	if (!warm_file_proof(&inv, key, epoch, path, &sed_start, &sed_end, &line_count)) {
		unmap_snapshot(&snap);
		return 0;
	}
	char command_hash[HASH_HEX];
	char command_input[128];
	snprintf(command_input, sizeof(command_input), "warm-file:%s", key);
	sha256_hex_str(command_input, command_hash);
	const unsigned char *content, *err;
	uint32_t content_len, err_len;
	int stored_exit_code;
	uint64_t native_wall_ms = 0;
	if (!snapshot_find(&snap, command_hash, epoch, HOT_KIND_WARM_FILE, &content, &content_len, &err, &err_len, &stored_exit_code, &native_wall_ms) ||
	    err_len != 0 || stored_exit_code != 0) {
		unmap_snapshot(&snap);
		return 0;
	}
	byte_buf out = {0};
	int exit_code = 0;
	int ok = 0;
	if (strcmp(inv.argv[0], "cat") == 0) {
		ok = shell_eval_copy_bytes(&out, content, content_len);
	} else if (strcmp(inv.argv[0], "sed") == 0) {
		ok = shell_append_sed_range(&out, content, content_len, sed_start, sed_end);
	} else if (strcmp(inv.argv[0], "head") == 0) {
		ok = shell_append_sed_range(&out, content, content_len, 1, line_count);
	} else if (strcmp(inv.argv[0], "tail") == 0) {
		ok = shell_append_tail_lines(&out, content, content_len, line_count);
	} else if (strcmp(inv.argv[0], "grep") == 0) {
		const char *pattern = NULL;
		const char *grep_path = NULL;
		int quiet = 0;
		int matched = 0;
		ok = parse_fixed_grep_args(&inv, &pattern, &grep_path, &quiet) &&
		     shell_append_fixed_grep(&out, content, content_len, pattern, quiet, &matched);
		exit_code = matched ? 0 : 1;
	}
	if (!ok) {
		bytes_free(&out);
		unmap_snapshot(&snap);
		return 0;
	}
	int emitted = emit_synthetic_buffer_replay(store_root, native_wall_ms, now_monotonic_ns(), &out, exit_code, file_actions, pid_out);
	bytes_free(&out);
	unmap_snapshot(&snap);
	return emitted;
}

static int envp_contains_ptr(char *const envp[], char *entry) {
	if (envp == NULL || entry == NULL) {
		return 0;
	}
	for (size_t i = 0; envp[i] != NULL; i++) {
		if (envp[i] == entry) {
			return 1;
		}
	}
	return 0;
}

static void exec_native_child(int use_path, const char *path, char *const argv[], char *const envp[]);
static int spawn_helper_for_argv(pid_t *pid, const char *trace_path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const command_argv[], char *const envp[]);
static int spawn_shell_ir_helper(int use_path, pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[], const char *command, const char *shell_flag);

static int spawn_shell_replay_child(int use_path, pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
	if (squire_preload_guard) {
		preload_trace("shell-spawn-skip-guard", path);
		return -1;
	}
	if (!preload_active()) {
		preload_trace("shell-spawn-skip-disabled", path);
		return -1;
	}
	if (!is_shell_launcher_name(path, argv)) {
		preload_trace("shell-spawn-skip-name", path);
		return -1;
	}
	if (attrp != NULL) {
		preload_trace("shell-spawn-skip-attr", path);
		return -1;
	}
	const char *command = shell_command_arg(argv);
	const char *shell_flag = shell_command_flag(argv);
	if (command == NULL) {
		preload_trace_shell_argv("shell-spawn-skip-argv", path, argv);
		return -1;
	}
	if (shell_flag != NULL && shell_command_composition_candidate(command)) {
		int helper_rc = spawn_shell_ir_helper(use_path, pid, path, file_actions, attrp, argv, envp, command, shell_flag);
		if (helper_rc >= 0) {
			preload_trace("shell-spawn-ir-helper", path);
			return helper_rc;
		}
	}
	int parsed_argc = 0;
	char *parsed_argv[MAX_ARGC];
	char parsed_storage[MAX_ARGC][PATH_BUF];
	if (!parse_simple_shell_command(command, &parsed_argc, parsed_argv, parsed_storage)) {
		return -1;
	}
	if (!preload_tool_candidate(parsed_argv[0], parsed_argv)) {
		return -1;
	}
	if (!parsed_shell_replay_env_compatible(parsed_argc, parsed_argv, envp)) {
		preload_trace("shell-spawn-skip-env", parsed_argv[0]);
		if (require_hit()) {
			fprintf(stderr, "squire preload: shell replay env incompatible\n");
			return 91;
		}
		return -1;
	}
	if (file_actions != NULL) {
		int helper_rc = spawn_helper_for_argv(pid, parsed_argv[0], file_actions, attrp, parsed_argv, envp);
		if (helper_rc >= 0) {
			preload_trace("shell-spawn-helper", parsed_argv[0]);
			return helper_rc;
		}
		return -1;
	}
	prepared_exact_replay prepared;
	preload_trace("shell-spawn-prepare", parsed_argv[0]);
	if (prepare_exact_replay(parsed_argc, parsed_argv, &prepared)) {
		preload_trace("shell-spawn-prepared", parsed_argv[0]);
		pid_t synthetic_pid;
		if (emit_synthetic_prepared_replay(&prepared, file_actions, &synthetic_pid)) {
			release_prepared_exact_replay(&prepared);
			preload_trace("shell-spawn-synthetic-child", parsed_argv[0]);
			*pid = synthetic_pid;
			return 0;
		}
		preload_trace("shell-spawn-fork-prepared", parsed_argv[0]);
		pid_t child = fork();
		if (child < 0) {
			release_prepared_exact_replay(&prepared);
			return errno;
		}
		if (child == 0) {
			preload_trace("shell-spawn-child-emit", parsed_argv[0]);
			if (file_actions != NULL && !apply_tracked_file_actions(file_actions)) {
				_exit(127);
			}
			emit_prepared_exact_replay(&prepared);
		}
		release_prepared_exact_replay(&prepared);
		preload_trace("shell-spawn-prepared-child", parsed_argv[0]);
		*pid = child;
		return 0;
	}

	pid_t child = fork();
	if (child < 0) {
		return errno;
	}
	if (child == 0) {
		if (file_actions != NULL && !apply_tracked_file_actions(file_actions)) {
			_exit(127);
		}
		squire_preload_guard = 1;
		if (!try_replay(parsed_argc, parsed_argv)) {
			if (require_hit()) {
				fprintf(stderr, "squire preload: shell hot snapshot miss\n");
				_exit(91);
			}
			exec_native_child(use_path, path, argv, envp);
		}
		_exit(0);
	}
	preload_trace("shell-spawn-child", parsed_argv[0]);
	*pid = child;
	return 0;
}

static void free_scrubbed_envp(char **scrubbed, char *const original[]) {
	if (scrubbed == NULL || scrubbed == original) {
		return;
	}
	for (size_t i = 0; scrubbed[i] != NULL; i++) {
		if (!envp_contains_ptr(original, scrubbed[i])) {
			free(scrubbed[i]);
		}
	}
	free(scrubbed);
}

static int native_spawn(int use_path, pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
	char **native_envp = scrub_preload_envp(envp);
	char resolved[PATH_BUF];
	const char *spawn_path = path;
	if (use_path && path != NULL && strchr(path, '/') == NULL) {
		char cwd[PATH_BUF];
		if (getcwd(cwd, sizeof(cwd)) == NULL || !resolve_executable(cwd, path, resolved)) {
			free_scrubbed_envp(native_envp, envp);
			return ENOENT;
		}
		spawn_path = resolved;
	}
	squire_preload_guard = 1;
	int rc = native_posix_spawn_call(pid, spawn_path, file_actions, attrp, argv, native_envp);
	squire_preload_guard = 0;
	free_scrubbed_envp(native_envp, envp);
	return rc;
}

static int spawn_helper_for_argv(pid_t *pid, const char *trace_path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const command_argv[], char *const envp[]) {
	if (attrp != NULL) {
		preload_trace("helper-skip-attr", trace_path);
		return -1;
	}
	const char *helper = getenv("SQUIRE_PRELOAD_HELPER");
	if (helper == NULL || helper[0] == '\0') {
		preload_trace("helper-skip-missing", trace_path);
		return -1;
	}
	int argc = count_argv(command_argv);
	if (argc <= 0) {
		preload_trace("helper-skip-argv", trace_path);
		return -1;
	}
	char **helper_argv = (char **)calloc((size_t)argc + 2, sizeof(char *));
	if (helper_argv == NULL) {
		preload_trace("helper-skip-alloc", trace_path);
		return ENOMEM;
	}
	helper_argv[0] = (char *)helper;
	for (int i = 0; i < argc; i++) {
		helper_argv[i + 1] = command_argv[i];
	}
	helper_argv[argc + 1] = NULL;
	char **helper_envp = scrub_preload_envp_for_helper(envp);
	preload_trace("helper-spawn-native", helper);
	squire_preload_guard = 1;
	int rc = native_posix_spawn_call(pid, helper, file_actions, attrp, helper_argv, helper_envp);
	squire_preload_guard = 0;
	free_scrubbed_envp(helper_envp, envp);
	free(helper_argv);
	return rc;
}

static int spawn_shell_ir_helper(int use_path, pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[], const char *command, const char *shell_flag) {
	if (attrp != NULL || pid == NULL || command == NULL || shell_flag == NULL) {
		return -1;
	}
	const char *helper = getenv("SQUIRE_PRELOAD_HELPER");
	if (helper == NULL || helper[0] == '\0') {
		preload_trace("shell-ir-helper-skip-missing", path);
		return -1;
	}
	char resolved[PATH_BUF];
	const char *shell_path = path;
	if (use_path && path != NULL && strchr(path, '/') == NULL) {
		char cwd[PATH_BUF];
		if (getcwd(cwd, sizeof(cwd)) == NULL || !resolve_executable(cwd, path, resolved)) {
			return -1;
		}
		shell_path = resolved;
	}
	if (shell_path == NULL || shell_path[0] == '\0') {
		return -1;
	}
	const char *shell_argv0 = argv != NULL && argv[0] != NULL ? argv[0] : base_name(shell_path);
	char *helper_argv[] = {
		(char *)helper,
		"--shell-ir",
		(char *)shell_path,
		(char *)shell_argv0,
		(char *)shell_flag,
		(char *)command,
		NULL,
	};
	char **helper_envp = scrub_preload_envp_for_helper(envp);
	preload_trace("shell-ir-helper-spawn-native", helper);
	squire_preload_guard = 1;
	int rc = native_posix_spawn_call(pid, helper, file_actions, attrp, helper_argv, helper_envp);
	squire_preload_guard = 0;
	free_scrubbed_envp(helper_envp, envp);
	return rc;
}

static int spawn_helper(int use_path, pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
	(void)use_path;
	return spawn_helper_for_argv(pid, path, file_actions, attrp, argv, envp);
}

static void exec_native_child(int use_path, const char *path, char *const argv[], char *const envp[]) {
	if (real_execve_ptr == NULL) {
		_exit(127);
	}
	char resolved[PATH_BUF];
	const char *native_path = path;
	if (use_path && path != NULL && strchr(path, '/') == NULL) {
		char cwd[PATH_BUF];
		if (getcwd(cwd, sizeof(cwd)) == NULL || !resolve_executable(cwd, path, resolved)) {
			_exit(127);
		}
		native_path = resolved;
	}
	char **native_envp = scrub_preload_envp(envp);
	native_execve_call(native_path, argv, native_envp);
	_exit(errno == ENOENT ? 127 : 126);
}

static int spawn_replay_child(int use_path, pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
	if (squire_preload_guard || !preload_active() ||
	    (envp != NULL && envp != environ) ||
	    !preload_tool_candidate(path, argv)) {
		return -1;
	}
	if (attrp != NULL) {
		return -1;
	}
	if (file_actions != NULL) {
		prepared_exact_replay prepared;
		preload_trace("spawn-file-actions-prepare", path);
		if (prepare_exact_replay(count_argv(argv), (char **)argv, &prepared)) {
			pid_t synthetic_pid;
			if (emit_synthetic_prepared_replay(&prepared, file_actions, &synthetic_pid)) {
				release_prepared_exact_replay(&prepared);
				preload_trace("spawn-synthetic-child", path);
				*pid = synthetic_pid;
				return 0;
			}
			release_prepared_exact_replay(&prepared);
		}
		pid_t warm_synthetic_pid;
		if (emit_synthetic_warm_file_replay(count_argv(argv), (char **)argv, file_actions, &warm_synthetic_pid)) {
			preload_trace("spawn-synthetic-warm-file", path);
			*pid = warm_synthetic_pid;
			return 0;
		}
		int helper_rc = spawn_helper(use_path, pid, path, file_actions, attrp, argv, envp);
		if (helper_rc >= 0) {
			preload_trace("spawn-helper", path);
			return helper_rc;
		}
		return -1;
	}
	prepared_exact_replay prepared;
	preload_trace("spawn-prepare", path);
	if (prepare_exact_replay(count_argv(argv), (char **)argv, &prepared)) {
		preload_trace("spawn-prepared", path);
		pid_t synthetic_pid;
		if (emit_synthetic_prepared_replay(&prepared, file_actions, &synthetic_pid)) {
			release_prepared_exact_replay(&prepared);
			*pid = synthetic_pid;
			return 0;
		}
		preload_trace("spawn-fork-prepared", path);
		pid_t child = fork();
		if (child < 0) {
			release_prepared_exact_replay(&prepared);
			return errno;
		}
		if (child == 0) {
			preload_trace("spawn-child-emit", path);
			if (file_actions != NULL && !apply_tracked_file_actions(file_actions)) {
				_exit(127);
			}
			emit_prepared_exact_replay(&prepared);
		}
		release_prepared_exact_replay(&prepared);
		*pid = child;
		return 0;
	}
	pid_t child = fork();
	if (child < 0) {
		return errno;
	}
	if (child == 0) {
		if (file_actions != NULL && !apply_tracked_file_actions(file_actions)) {
			_exit(127);
		}
		squire_preload_guard = 1;
		if (!try_replay(count_argv(argv), (char **)argv)) {
			if (require_hit()) {
				fprintf(stderr, "squire preload: hot snapshot miss\n");
				_exit(91);
			}
			exec_native_child(use_path, path, argv, envp);
		}
		_exit(0);
	}
	*pid = child;
	return 0;
}

int squire_preload_file_actions_init(posix_spawn_file_actions_t *actions) {
	preload_trace("file-actions-init", NULL);
	if (real_file_actions_init_ptr == NULL || real_file_actions_init_ptr == squire_preload_file_actions_init) {
		preload_trace("file-actions-init-missing", NULL);
		return ENOSYS;
	}
	int rc = real_file_actions_init_ptr(actions);
	if (rc == 0) {
		tracked_file_actions_record *record = ensure_file_actions_record(actions);
		if (record != NULL) {
			record->unsupported = 0;
			record->count = 0;
		}
	}
	return rc;
}

int squire_preload_file_actions_destroy(posix_spawn_file_actions_t *actions) {
	preload_trace("file-actions-destroy", NULL);
	if (real_file_actions_destroy_ptr == NULL || real_file_actions_destroy_ptr == squire_preload_file_actions_destroy) {
		preload_trace("file-actions-destroy-missing", NULL);
		return ENOSYS;
	}
	int rc = real_file_actions_destroy_ptr(actions);
	if (rc == 0) {
		remove_file_actions_record(actions);
	}
	return rc;
}

int squire_preload_file_actions_addclose(posix_spawn_file_actions_t *actions, int fd) {
	preload_trace("file-actions-addclose", NULL);
	if (real_file_actions_addclose_ptr == NULL || real_file_actions_addclose_ptr == squire_preload_file_actions_addclose) {
		preload_trace("file-actions-addclose-missing", NULL);
		return ENOSYS;
	}
	int rc = real_file_actions_addclose_ptr(actions, fd);
	if (rc == 0) {
		append_file_action(actions, FILE_ACTION_CLOSE, fd, -1);
	}
	return rc;
}

int squire_preload_file_actions_adddup2(posix_spawn_file_actions_t *actions, int fd, int newfd) {
	preload_trace("file-actions-adddup2", NULL);
	if (real_file_actions_adddup2_ptr == NULL || real_file_actions_adddup2_ptr == squire_preload_file_actions_adddup2) {
		preload_trace("file-actions-adddup2-missing", NULL);
		return ENOSYS;
	}
	int rc = real_file_actions_adddup2_ptr(actions, fd, newfd);
	if (rc == 0) {
		append_file_action(actions, FILE_ACTION_DUP2, fd, newfd);
	}
	return rc;
}

int squire_preload_execve(const char *path, char *const argv[], char *const envp[]) {
	preload_trace("execve", path);
	if (squire_preload_trace_enabled && path != NULL && strstr(path, "sh") != NULL) {
		preload_trace_shell_argv("execve-shell-probe", path, argv);
	}
	if (maybe_replay_shell_execve(path, argv, envp)) {
		return 0;
	}
	if (envp != NULL && envp != environ) {
		return native_execve_call(path, argv, scrub_preload_envp_in_place(envp));
	}
	maybe_replay_exec(path, argv, envp);
	char **native_envp = scrub_preload_envp(envp);
	return native_execve_call(path, argv, native_envp);
}

int squire_preload_execv(const char *path, char *const argv[]) {
	preload_trace("execv", path);
	maybe_replay_exec(path, argv, environ);
	return native_execve_call(path, argv, environ);
}

int squire_preload_execvp(const char *file, char *const argv[]) {
	preload_trace("execvp", file);
	maybe_replay_exec(file, argv, environ);
	char resolved[PATH_BUF];
	const char *native_path = file;
	if (file != NULL && strchr(file, '/') == NULL) {
		char cwd[PATH_BUF];
		if (getcwd(cwd, sizeof(cwd)) == NULL || !resolve_executable(cwd, file, resolved)) {
			errno = ENOENT;
			return -1;
		}
		native_path = resolved;
	}
	return native_execve_call(native_path, argv, environ);
}

int squire_preload_posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
	preload_trace("posix_spawn", path);
	if (squire_preload_guard) {
		return native_posix_spawn_call(pid, path, file_actions, attrp, argv, envp);
	}
	int shell_replay_rc = spawn_shell_replay_child(0, pid, path, file_actions, attrp, argv, envp);
	if (shell_replay_rc >= 0) {
		return shell_replay_rc;
	}
	int replay_rc = spawn_replay_child(0, pid, path, file_actions, attrp, argv, envp);
	if (replay_rc >= 0) {
		return replay_rc;
	}
	return native_spawn(0, pid, path, file_actions, attrp, argv, envp);
}

int squire_preload_posix_spawnp(pid_t *pid, const char *file, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
	preload_trace("posix_spawnp", file);
	if (squire_preload_guard) {
		preload_trace("posix_spawnp-skip-guard", file);
		char resolved[PATH_BUF];
		const char *spawn_path = file;
		if (file != NULL && strchr(file, '/') == NULL) {
			char cwd[PATH_BUF];
			if (getcwd(cwd, sizeof(cwd)) == NULL || !resolve_executable(cwd, file, resolved)) {
				return ENOENT;
			}
			spawn_path = resolved;
		}
		return native_posix_spawn_call(pid, spawn_path, file_actions, attrp, argv, envp);
	}
	int shell_replay_rc = spawn_shell_replay_child(1, pid, file, file_actions, attrp, argv, envp);
	if (shell_replay_rc >= 0) {
		return shell_replay_rc;
	}
	int replay_rc = spawn_replay_child(1, pid, file, file_actions, attrp, argv, envp);
	if (replay_rc >= 0) {
		return replay_rc;
	}
	return native_spawn(1, pid, file, file_actions, attrp, argv, envp);
}

pid_t squire_preload_waitpid(pid_t pid, int *status, int options) {
	pid_t synthetic = consume_synthetic_child(pid, status);
	if (synthetic > 0) {
		return synthetic;
	}
	return native_waitpid_call(pid, status, options);
}

pid_t squire_preload_wait(int *status) {
	pid_t synthetic = consume_synthetic_child(-1, status);
	if (synthetic > 0) {
		return synthetic;
	}
	return native_waitpid_call(-1, status, 0);
}

#if defined(__APPLE__)
#define SQUIRE_INTERPOSE(replacement, replacee)                                                   \
	__attribute__((used)) static struct {                                                        \
		const void *replacement;                                                                 \
		const void *replacee;                                                                    \
	} squire_interpose_##replacee __attribute__((section("__DATA,__interpose"))) = {              \
		(const void *)(replacement), (const void *)(replacee)                                     \
	}

SQUIRE_INTERPOSE(squire_preload_execve, execve);
SQUIRE_INTERPOSE(squire_preload_execv, execv);
SQUIRE_INTERPOSE(squire_preload_execvp, execvp);
SQUIRE_INTERPOSE(squire_preload_posix_spawn, posix_spawn);
SQUIRE_INTERPOSE(squire_preload_posix_spawnp, posix_spawnp);
SQUIRE_INTERPOSE(squire_preload_file_actions_init, posix_spawn_file_actions_init);
SQUIRE_INTERPOSE(squire_preload_file_actions_destroy, posix_spawn_file_actions_destroy);
SQUIRE_INTERPOSE(squire_preload_file_actions_addclose, posix_spawn_file_actions_addclose);
SQUIRE_INTERPOSE(squire_preload_file_actions_adddup2, posix_spawn_file_actions_adddup2);
SQUIRE_INTERPOSE(squire_preload_waitpid, waitpid);
SQUIRE_INTERPOSE(squire_preload_wait, wait);
#else
int execve(const char *path, char *const argv[], char *const envp[]) {
	return squire_preload_execve(path, argv, envp);
}

int execv(const char *path, char *const argv[]) {
	return squire_preload_execv(path, argv);
}

int execvp(const char *file, char *const argv[]) {
	return squire_preload_execvp(file, argv);
}

int posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
	return squire_preload_posix_spawn(pid, path, file_actions, attrp, argv, envp);
}

int posix_spawnp(pid_t *pid, const char *file, const posix_spawn_file_actions_t *file_actions, const posix_spawnattr_t *attrp, char *const argv[], char *const envp[]) {
	return squire_preload_posix_spawnp(pid, file, file_actions, attrp, argv, envp);
}

int posix_spawn_file_actions_init(posix_spawn_file_actions_t *actions) {
	return squire_preload_file_actions_init(actions);
}

int posix_spawn_file_actions_destroy(posix_spawn_file_actions_t *actions) {
	return squire_preload_file_actions_destroy(actions);
}

int posix_spawn_file_actions_addclose(posix_spawn_file_actions_t *actions, int fd) {
	return squire_preload_file_actions_addclose(actions, fd);
}

int posix_spawn_file_actions_adddup2(posix_spawn_file_actions_t *actions, int fd, int newfd) {
	return squire_preload_file_actions_adddup2(actions, fd, newfd);
}

pid_t waitpid(pid_t pid, int *status, int options) {
	return squire_preload_waitpid(pid, status, options);
}

pid_t wait(int *status) {
	return squire_preload_wait(status);
}

#endif
