// Internal helper for Squire's scoped preload transport.
//
// The preload library uses this executable when an intercepted posix_spawn call
// includes file actions, such as stdout/stderr pipes. The caller's native
// posix_spawn applies those file actions to this helper; the helper then writes
// exact replay bytes to the already-wired descriptors or execs the native
// command on any miss. It is not a PATH shim and is not invoked by agents.

#ifndef SQUIRE_MMAP_NO_MAIN
#define SQUIRE_MMAP_NO_MAIN 1
#endif
#define SQUIRE_MMAP_HELPER_REAL_EXEC 1
#include "squire_mmap_shim.c"

#define HELPER_SHELL_MAX_TOKENS 96
#define HELPER_SHELL_MAX_NODES 64
#define HELPER_SHELL_MAX_ARGS 16
#define HELPER_SHELL_WORD_BUF 512

typedef enum {
	HELPER_TOKEN_WORD = 1,
	HELPER_TOKEN_PIPE,
	HELPER_TOKEN_AND,
	HELPER_TOKEN_SEMI,
	HELPER_TOKEN_LPAREN,
	HELPER_TOKEN_RPAREN,
	HELPER_TOKEN_REDIR_NULL,
} helper_token_kind;

typedef struct {
	helper_token_kind kind;
	char text[HELPER_SHELL_WORD_BUF];
	int fd;
} helper_token;

typedef enum {
	HELPER_NODE_EXEC = 1,
	HELPER_NODE_PIPE,
	HELPER_NODE_AND,
	HELPER_NODE_SEQ,
	HELPER_NODE_REDIR_NULL,
} helper_node_kind;

typedef struct {
	helper_node_kind kind;
	int left;
	int right;
	int fd;
	int argc;
	char argv[HELPER_SHELL_MAX_ARGS][HELPER_SHELL_WORD_BUF];
} helper_node;

typedef struct {
	helper_node nodes[HELPER_SHELL_MAX_NODES];
	int count;
	int root;
} helper_plan;

typedef struct {
	helper_token tokens[HELPER_SHELL_MAX_TOKENS];
	int count;
	int pos;
	helper_plan plan;
} helper_parser;

typedef struct {
	byte_buf stdout_buf;
	byte_buf stderr_buf;
	int exit_code;
} helper_result;

static int helper_word_allowed(unsigned char c) {
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

static int helper_word_meta(unsigned char c) {
	return c == '\0' || c == ' ' || c == '\t' || c == '\n' ||
	       c == '|' || c == '&' || c == ';' || c == '(' || c == ')' ||
	       c == '<' || c == '>';
}

static int helper_token_add(helper_token *tokens, int *count, helper_token_kind kind, const char *text, size_t text_len, int fd) {
	if (*count >= HELPER_SHELL_MAX_TOKENS - 1 || text_len >= HELPER_SHELL_WORD_BUF) {
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

static int helper_tokenize_redir_null(const unsigned char **cursor, helper_token *tokens, int *count, int fd) {
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
	if (!helper_word_meta(*p)) {
		return 0;
	}
	*cursor = p;
	return helper_token_add(tokens, count, HELPER_TOKEN_REDIR_NULL, NULL, 0, fd);
}

static int helper_tokenize(const char *command, helper_token *tokens, int *count) {
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
			if (p[1] == '|' || !helper_token_add(tokens, count, HELPER_TOKEN_PIPE, NULL, 0, 0)) {
				return 0;
			}
			p++;
			continue;
		}
		if (*p == '&') {
			if (p[1] != '&' || !helper_token_add(tokens, count, HELPER_TOKEN_AND, NULL, 0, 0)) {
				return 0;
			}
			p += 2;
			continue;
		}
		if (*p == ';') {
			if (!helper_token_add(tokens, count, HELPER_TOKEN_SEMI, NULL, 0, 0)) {
				return 0;
			}
			p++;
			continue;
		}
		if (*p == '(') {
			if (!helper_token_add(tokens, count, HELPER_TOKEN_LPAREN, NULL, 0, 0)) {
				return 0;
			}
			p++;
			continue;
		}
		if (*p == ')') {
			if (!helper_token_add(tokens, count, HELPER_TOKEN_RPAREN, NULL, 0, 0)) {
				return 0;
			}
			p++;
			continue;
		}
		if (*p == '>') {
			if (!helper_tokenize_redir_null(&p, tokens, count, 1)) {
				return 0;
			}
			continue;
		}
		if ((*p == '1' || *p == '2') && p[1] == '>') {
			int fd = *p == '2' ? 2 : 1;
			p++;
			if (!helper_tokenize_redir_null(&p, tokens, count, fd)) {
				return 0;
			}
			continue;
		}
		char word[HELPER_SHELL_WORD_BUF];
		size_t len = 0;
		while (!helper_word_meta(*p)) {
			if (*p == '\'' || *p == '"') {
				unsigned char quote = *p++;
				while (*p != '\0' && *p != quote) {
					if (!helper_word_allowed(*p) || len + 1 >= sizeof(word)) {
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
			if (!helper_word_allowed(*p) || *p == '\'' || *p == '"' || len + 1 >= sizeof(word)) {
				return 0;
			}
			word[len++] = (char)*p++;
		}
		if (len == 0 || !helper_token_add(tokens, count, HELPER_TOKEN_WORD, word, len, 0)) {
			return 0;
		}
	}
	return *count > 0;
}

static int helper_add_node(helper_parser *p, helper_node_kind kind, int left, int right, int fd) {
	if (p->plan.count >= HELPER_SHELL_MAX_NODES) {
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

static helper_token *helper_peek(helper_parser *p) {
	return p->pos >= p->count ? NULL : &p->tokens[p->pos];
}

static int helper_accept(helper_parser *p, helper_token_kind kind) {
	helper_token *tok = helper_peek(p);
	if (tok == NULL || tok->kind != kind) {
		return 0;
	}
	p->pos++;
	return 1;
}

static int helper_parse_sequence(helper_parser *p, int *node_out);

static int helper_parse_primary(helper_parser *p, int *node_out) {
	helper_token *tok = helper_peek(p);
	if (tok == NULL) {
		return 0;
	}
	int node = -1;
	if (helper_accept(p, HELPER_TOKEN_LPAREN)) {
		if (!helper_parse_sequence(p, &node) || !helper_accept(p, HELPER_TOKEN_RPAREN)) {
			return 0;
		}
	} else if (tok->kind == HELPER_TOKEN_WORD) {
		node = helper_add_node(p, HELPER_NODE_EXEC, -1, -1, 0);
		if (node < 0) {
			return 0;
		}
		while ((tok = helper_peek(p)) != NULL && tok->kind == HELPER_TOKEN_WORD) {
			if (p->plan.nodes[node].argc >= HELPER_SHELL_MAX_ARGS - 1) {
				return 0;
			}
			snprintf(p->plan.nodes[node].argv[p->plan.nodes[node].argc], HELPER_SHELL_WORD_BUF, "%s", tok->text);
			p->plan.nodes[node].argc++;
			p->pos++;
		}
		if (p->plan.nodes[node].argc <= 0 || strchr(p->plan.nodes[node].argv[0], '=') != NULL) {
			return 0;
		}
	} else {
		return 0;
	}
	while ((tok = helper_peek(p)) != NULL && tok->kind == HELPER_TOKEN_REDIR_NULL) {
		p->pos++;
		node = helper_add_node(p, HELPER_NODE_REDIR_NULL, node, -1, tok->fd);
		if (node < 0) {
			return 0;
		}
	}
	*node_out = node;
	return 1;
}

static int helper_parse_pipeline(helper_parser *p, int *node_out) {
	int node = -1;
	if (!helper_parse_primary(p, &node)) {
		return 0;
	}
	while (helper_accept(p, HELPER_TOKEN_PIPE)) {
		int right = -1;
		if (!helper_parse_primary(p, &right)) {
			return 0;
		}
		node = helper_add_node(p, HELPER_NODE_PIPE, node, right, 0);
		if (node < 0) {
			return 0;
		}
	}
	*node_out = node;
	return 1;
}

static int helper_parse_and(helper_parser *p, int *node_out) {
	int node = -1;
	if (!helper_parse_pipeline(p, &node)) {
		return 0;
	}
	while (helper_accept(p, HELPER_TOKEN_AND)) {
		int right = -1;
		if (!helper_parse_pipeline(p, &right)) {
			return 0;
		}
		node = helper_add_node(p, HELPER_NODE_AND, node, right, 0);
		if (node < 0) {
			return 0;
		}
	}
	*node_out = node;
	return 1;
}

static int helper_parse_sequence(helper_parser *p, int *node_out) {
	int node = -1;
	if (!helper_parse_and(p, &node)) {
		return 0;
	}
	while (helper_accept(p, HELPER_TOKEN_SEMI)) {
		if (helper_peek(p) == NULL || helper_peek(p)->kind == HELPER_TOKEN_RPAREN) {
			break;
		}
		int right = -1;
		if (!helper_parse_and(p, &right)) {
			return 0;
		}
		node = helper_add_node(p, HELPER_NODE_SEQ, node, right, 0);
		if (node < 0) {
			return 0;
		}
	}
	*node_out = node;
	return 1;
}

static int helper_parse_shell_plan(const char *command, helper_plan *plan) {
	if (command == NULL || plan == NULL) {
		return 0;
	}
	helper_parser *p = (helper_parser *)calloc(1, sizeof(*p));
	if (p == NULL) {
		return 0;
	}
	int root = -1;
	if (!helper_tokenize(command, p->tokens, &p->count) ||
	    !helper_parse_sequence(p, &root) ||
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

static void helper_result_free(helper_result *res) {
	if (res == NULL) {
		return;
	}
	free(res->stdout_buf.data);
	free(res->stderr_buf.data);
	memset(res, 0, sizeof(*res));
}

static int helper_copy_bytes(byte_buf *dst, const unsigned char *data, uint32_t len) {
	return len == 0 || bytes_append(dst, data, len);
}

static int helper_append_result(byte_buf *dst, const byte_buf *src) {
	return src == NULL || src->len == 0 || bytes_append(dst, src->data, src->len);
}

static int helper_append_sed_range(byte_buf *out, const unsigned char *content, uint32_t len, int start, int end) {
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
		if (line >= start && line <= end && !helper_copy_bytes(out, content + offset, write_end - offset)) {
			return 0;
		}
		if (line > end) {
			break;
		}
		line++;
		offset = write_end;
	}
	return 1;
}

static int helper_append_tail_lines(byte_buf *out, const unsigned char *content, uint32_t len, int count) {
	int total = count_lines(content, len);
	int start = total - count + 1;
	if (start < 1) {
		start = 1;
	}
	return helper_append_sed_range(out, content, len, start, total);
}

static int helper_append_fixed_grep(byte_buf *out, const unsigned char *content, uint32_t len, const char *pattern, int quiet, int *matched) {
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
			if (!helper_copy_bytes(out, content + offset, line_end - offset) ||
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

static int helper_append_decimal_int(byte_buf *out, int value) {
	char buf[32];
	int n = snprintf(buf, sizeof(buf), "%d", value);
	if (n <= 0 || n >= (int)sizeof(buf)) {
		return 0;
	}
	return bytes_append(out, (const unsigned char *)buf, (size_t)n);
}

static int helper_append_wc_line_count(byte_buf *out, const unsigned char *content, size_t len) {
	size_t lines = 0;
	for (size_t i = 0; i < len; i++) {
		if (content[i] == '\n') {
			lines++;
		}
	}
	char buf[64];
#if defined(__APPLE__)
	int n = snprintf(buf, sizeof(buf), "%8zu\n", lines);
#else
	int n = snprintf(buf, sizeof(buf), "%zu\n", lines);
#endif
	if (n <= 0 || n >= (int)sizeof(buf)) {
		return 0;
	}
	return bytes_append(out, (const unsigned char *)buf, (size_t)n);
}

static int helper_append_sorted_lines(byte_buf *out, const unsigned char *content, size_t len) {
	if (len == 0) {
		return 1;
	}
	for (size_t i = 0; i < len; i++) {
		if (content[i] == '\0') {
			return 0;
		}
	}
	string_list lines = {0};
	size_t offset = 0;
	while (offset < len) {
		size_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		if (!list_add_bytes(&lines, content + offset, line_end - offset)) {
			list_free(&lines);
			return 0;
		}
		offset = line_end < len && content[line_end] == '\n' ? line_end + 1 : line_end;
	}
	list_sort(&lines);
	int ok = 1;
	for (size_t i = 0; i < lines.len; i++) {
		if (!bytes_append(out, lines.items[i].data, lines.items[i].len) ||
		    !bytes_append_byte(out, '\n')) {
			ok = 0;
			break;
		}
		if (out->len > MAX_FAST_OUTPUT_BYTES) {
			ok = 0;
			break;
		}
	}
	list_free(&lines);
	return ok;
}

static int helper_append_fixed_rg(byte_buf *out, const unsigned char *content, uint32_t len, const char *pattern, int quiet, int line_number, int *matched) {
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
		uint32_t line_len = line_end - offset;
		if (mem_contains_bytes(content + offset, line_len, (const unsigned char *)pattern, pattern_len)) {
			*matched = 1;
			if (quiet) {
				return 1;
			}
			if (line_number && (!helper_append_decimal_int(out, line) || !bytes_append_byte(out, ':'))) {
				return 0;
			}
			if (!helper_copy_bytes(out, content + offset, line_len) ||
			    !bytes_append_byte(out, '\n')) {
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

static int helper_parse_line_count_arg(int argc, char argv[HELPER_SHELL_MAX_ARGS][HELPER_SHELL_WORD_BUF], int *count) {
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

static int helper_parse_stdin_grep(int argc, char argv[HELPER_SHELL_MAX_ARGS][HELPER_SHELL_WORD_BUF], const char **pattern, int *quiet) {
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
		if (argv[i][0] == '-' || *pattern != NULL) {
			return 0;
		}
		*pattern = argv[i];
	}
	return fixed && *pattern != NULL;
}

static int helper_eval_node(helper_plan *plan, int idx, const byte_buf *input, helper_result *out, long long replay_start_ns, const char *cwd);

static int helper_eval_filter(helper_node *node, const byte_buf *input, helper_result *out) {
	const char *name = base_name(node->argv[0]);
	mmap_trace_path("shell-ir-helper-filter", name);
	if (strcmp(name, "cat") == 0 && node->argc == 1) {
		out->exit_code = 0;
		return helper_append_result(&out->stdout_buf, input);
	}
	int count = 0;
	if (strcmp(name, "head") == 0 && helper_parse_line_count_arg(node->argc, node->argv, &count)) {
		out->exit_code = 0;
		return helper_append_sed_range(&out->stdout_buf, input->data, (uint32_t)input->len, 1, count);
	}
	if (strcmp(name, "tail") == 0 && helper_parse_line_count_arg(node->argc, node->argv, &count)) {
		out->exit_code = 0;
		return helper_append_tail_lines(&out->stdout_buf, input->data, (uint32_t)input->len, count);
	}
	const char *pattern = NULL;
	int quiet = 0;
	if (strcmp(name, "grep") == 0 && helper_parse_stdin_grep(node->argc, node->argv, &pattern, &quiet)) {
		int matched = 0;
		if (!helper_append_fixed_grep(&out->stdout_buf, input->data, (uint32_t)input->len, pattern, quiet, &matched)) {
			return 0;
		}
		out->exit_code = matched ? 0 : 1;
		return 1;
	}
	if (strcmp(name, "wc") == 0 && node->argc == 2 && strcmp(node->argv[1], "-l") == 0) {
		out->exit_code = 0;
		return helper_append_wc_line_count(&out->stdout_buf, input->data, input->len);
	}
	if (strcmp(name, "sort") == 0 && node->argc == 1) {
		out->exit_code = 0;
		return helper_append_sorted_lines(&out->stdout_buf, input->data, input->len);
	}
	return 0;
}

static int helper_tool_candidate(const char *path, char *const argv[]) {
	const char *tool = argv != NULL && argv[0] != NULL ? base_name(argv[0]) : base_name(path);
	if (tool == NULL) {
		return 0;
	}
	return strcmp(tool, "git") == 0 ||
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
	       strcmp(tool, "cat") == 0 ||
	       strcmp(tool, "sed") == 0 ||
	       strcmp(tool, "head") == 0 ||
	       strcmp(tool, "tail") == 0;
}

static int helper_prepare_warm_file_at_cwd(const char *cwd, int argc, char **argv, helper_result *out, long long replay_start_ns) {
	mmap_trace_path("shell-ir-helper-warm-file-enter", argv != NULL && argc > 0 ? argv[0] : NULL);
	policy_invocation inv;
	if (!normalize_invocation_at_cwd(cwd, argc, argv, &inv) || !is_warm_file_candidate(&inv) || !warm_file_replay_enabled()) {
		mmap_trace_path("shell-ir-helper-warm-file-skip-policy", argv != NULL && argc > 0 ? argv[0] : NULL);
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
		ok = helper_copy_bytes(&out->stdout_buf, content, content_len);
		out->exit_code = 0;
	} else if (strcmp(inv.argv[0], "sed") == 0) {
		ok = helper_append_sed_range(&out->stdout_buf, content, content_len, sed_start, sed_end);
		out->exit_code = 0;
	} else if (strcmp(inv.argv[0], "head") == 0) {
		ok = helper_append_sed_range(&out->stdout_buf, content, content_len, 1, line_count);
		out->exit_code = 0;
	} else if (strcmp(inv.argv[0], "tail") == 0) {
		ok = helper_append_tail_lines(&out->stdout_buf, content, content_len, line_count);
		out->exit_code = 0;
	} else if (strcmp(inv.argv[0], "grep") == 0) {
		const char *pattern = NULL;
		const char *grep_path = NULL;
		int quiet = 0;
		int matched = 0;
		ok = parse_fixed_grep_args(&inv, &pattern, &grep_path, &quiet) &&
		     helper_append_fixed_grep(&out->stdout_buf, content, content_len, pattern, quiet, &matched);
		out->exit_code = matched ? 0 : 1;
	} else if (strcmp(inv.argv[0], "rg") == 0) {
		const char *pattern = NULL;
		const char *rg_path = NULL;
		int quiet = 0;
		int line_number = 0;
		int matched = 0;
		ok = parse_fixed_rg_args(&inv, &pattern, &rg_path, &quiet, &line_number) &&
		     helper_append_fixed_rg(&out->stdout_buf, content, content_len, pattern, quiet, line_number, &matched);
		out->exit_code = matched ? 0 : 1;
	}
	if (ok) {
		mmap_trace_path("shell-ir-helper-warm-file-hit", inv.argv[0]);
		record_hot_replay_event(store_root, (long long)native_wall_ms, replay_start_ns);
	}
	unmap_snapshot(&snap);
	return ok;
}

static int helper_prepare_warm_file(int argc, char **argv, helper_result *out, long long replay_start_ns) {
	return helper_prepare_warm_file_at_cwd(NULL, argc, argv, out, replay_start_ns);
}

static int helper_eval_source(helper_node *node, helper_result *out, long long replay_start_ns, const char *cwd) {
	char *argv[HELPER_SHELL_MAX_ARGS];
	for (int i = 0; i < node->argc; i++) {
		argv[i] = node->argv[i];
	}
	argv[node->argc] = NULL;
	mmap_trace_path("shell-ir-helper-source", argv[0]);
	if (!helper_tool_candidate(argv[0], argv)) {
		mmap_trace_path("shell-ir-helper-source-skip-policy", argv[0]);
		return 0;
	}
	prepared_exact_replay prepared;
	mmap_trace_path("shell-ir-helper-source-prepare", argv[0]);
	if (prepare_exact_replay_at_cwd(cwd, node->argc, argv, &prepared)) {
		mmap_trace_path("shell-ir-helper-source-hit", argv[0]);
		int ok = helper_copy_bytes(&out->stdout_buf, prepared.stdout_data, prepared.stdout_len) &&
		         helper_copy_bytes(&out->stderr_buf, prepared.stderr_data, prepared.stderr_len);
		out->exit_code = prepared.exit_code;
		if (ok) {
			record_hot_replay_event(prepared.store_root, (long long)prepared.native_wall_ms, replay_start_ns);
		}
		release_prepared_exact_replay(&prepared);
		return ok;
	}
	mmap_trace_path("shell-ir-helper-source-exact-miss", argv[0]);
	return helper_prepare_warm_file_at_cwd(cwd, node->argc, argv, out, replay_start_ns);
}

static int helper_eval_binary(helper_plan *plan, int left_idx, int right_idx, const byte_buf *input, helper_result *out, long long replay_start_ns, helper_node_kind kind, const char *cwd) {
	helper_result left = {0};
	if (!helper_eval_node(plan, left_idx, input, &left, replay_start_ns, cwd)) {
		return 0;
	}
	if (kind == HELPER_NODE_AND && left.exit_code != 0) {
		*out = left;
		return 1;
	}
	if (kind == HELPER_NODE_PIPE) {
		helper_result right = {0};
		if (!helper_eval_node(plan, right_idx, &left.stdout_buf, &right, replay_start_ns, cwd)) {
			helper_result_free(&left);
			return 0;
		}
		int ok = helper_append_result(&out->stderr_buf, &left.stderr_buf) &&
		         helper_append_result(&out->stderr_buf, &right.stderr_buf) &&
		         helper_append_result(&out->stdout_buf, &right.stdout_buf);
		out->exit_code = right.exit_code;
		helper_result_free(&left);
		helper_result_free(&right);
		return ok;
	}
	helper_result right = {0};
	if (!helper_eval_node(plan, right_idx, input, &right, replay_start_ns, cwd)) {
		helper_result_free(&left);
		return 0;
	}
	int ok = helper_append_result(&out->stdout_buf, &left.stdout_buf) &&
	         helper_append_result(&out->stderr_buf, &left.stderr_buf) &&
	         helper_append_result(&out->stdout_buf, &right.stdout_buf) &&
	         helper_append_result(&out->stderr_buf, &right.stderr_buf);
	out->exit_code = right.exit_code;
	helper_result_free(&left);
	helper_result_free(&right);
	return ok;
}

static int helper_eval_node(helper_plan *plan, int idx, const byte_buf *input, helper_result *out, long long replay_start_ns, const char *cwd) {
	if (plan == NULL || out == NULL || idx < 0 || idx >= plan->count) {
		return 0;
	}
	helper_node *node = &plan->nodes[idx];
	switch (node->kind) {
	case HELPER_NODE_EXEC:
		return input != NULL ? helper_eval_filter(node, input, out) : helper_eval_source(node, out, replay_start_ns, cwd);
	case HELPER_NODE_PIPE:
	case HELPER_NODE_AND:
	case HELPER_NODE_SEQ:
		return helper_eval_binary(plan, node->left, node->right, input, out, replay_start_ns, node->kind, cwd);
	case HELPER_NODE_REDIR_NULL:
		if (!helper_eval_node(plan, node->left, input, out, replay_start_ns, cwd)) {
			return 0;
		}
		if (node->fd == 1) {
			free(out->stdout_buf.data);
			memset(&out->stdout_buf, 0, sizeof(out->stdout_buf));
			return 1;
		}
		if (node->fd == 2) {
			free(out->stderr_buf.data);
			memset(&out->stderr_buf, 0, sizeof(out->stderr_buf));
			return 1;
		}
		return 0;
	default:
		return 0;
	}
}

static int helper_eval_shell_ir_at_cwd(const char *cwd, const char *command, helper_result *res, long long replay_start_ns) {
	mmap_trace_path("shell-ir-helper-run-enter", NULL);
	if (res == NULL) {
		return 0;
	}
	memset(res, 0, sizeof(*res));
	helper_plan *plan = (helper_plan *)calloc(1, sizeof(*plan));
	if (plan == NULL) {
		return 0;
	}
	if (!helper_parse_shell_plan(command, plan)) {
		mmap_trace_path("shell-ir-helper-parse-miss", NULL);
		free(plan);
		return 0;
	}
	mmap_trace_path("shell-ir-helper-parse-ok", NULL);
	if (!helper_eval_node(plan, plan->root, NULL, res, replay_start_ns, cwd)) {
		mmap_trace_path("shell-ir-helper-eval-miss", NULL);
		helper_result_free(res);
		free(plan);
		return 0;
	}
	free(plan);
	mmap_trace_path("shell-ir-helper-eval-ok", NULL);
	if (res->stdout_buf.len > MAX_FAST_OUTPUT_BYTES || res->stderr_buf.len > MAX_FAST_OUTPUT_BYTES) {
		helper_result_free(res);
		return 0;
	}
	return 1;
}

static int helper_run_shell_ir_at_cwd(const char *cwd, const char *command) {
	helper_result res = {0};
	if (!helper_eval_shell_ir_at_cwd(cwd, command, &res, now_monotonic_ns())) {
		return -1;
	}
	if (res.stdout_buf.len > 0 && !write_all(STDOUT_FILENO, res.stdout_buf.data, res.stdout_buf.len)) {
		helper_result_free(&res);
		return 127;
	}
	mmap_trace_path("shell-ir-helper-emit-ok", NULL);
	if (res.stderr_buf.len > 0 && !write_all(STDERR_FILENO, res.stderr_buf.data, res.stderr_buf.len)) {
		helper_result_free(&res);
		return 127;
	}
	int exit_code = res.exit_code;
	helper_result_free(&res);
	return exit_code;
}

static int helper_run_shell_ir(const char *command) {
	return helper_run_shell_ir_at_cwd(NULL, command);
}

static int helper_exec_shell_fallback(const char *shell_path, const char *shell_argv0, const char *shell_flag, const char *command) {
	if (shell_path == NULL || shell_argv0 == NULL || shell_flag == NULL || command == NULL) {
		return 127;
	}
	char *const shell_argv[] = {(char *)shell_argv0, (char *)shell_flag, (char *)command, NULL};
	execv(shell_path, shell_argv);
	return errno == ENOENT ? 127 : 126;
}

#ifndef SQUIRE_PRELOAD_HELPER_NO_MAIN
int main(int argc, char **argv) {
	if (argc >= 6 && strcmp(argv[1], "--shell-ir") == 0) {
		int rc = helper_run_shell_ir(argv[5]);
		if (rc >= 0) {
			return rc;
		}
		if (getenv("SQUIRE_SHIM_REQUIRE_HIT") != NULL) {
			fprintf(stderr, "squire preload helper: shell plan miss\n");
			return 91;
		}
		return helper_exec_shell_fallback(argv[2], argv[3], argv[4], argv[5]);
	}
	if (argc < 2) {
		fprintf(stderr, "squire preload helper: missing command\n");
		return 127;
	}
	int command_argc = argc - 1;
	char **command_argv = &argv[1];
	if (!try_replay(command_argc, command_argv)) {
		if (getenv("SQUIRE_SHIM_REQUIRE_HIT") != NULL) {
			fprintf(stderr, "squire preload helper: hot snapshot miss\n");
			return 91;
		}
		exec_real_command(command_argc, command_argv);
	}
	return 0;
}
#endif
