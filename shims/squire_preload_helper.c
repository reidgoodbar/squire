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

#include <glob.h>

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
	int unquoted_glob;
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
	int stderr_discarded;
	uint64_t shell_glob_mask;
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
	char replay_store_root[PATH_BUF];
	long long replay_start_ns;
	long long native_wall_ms;
	int replay_sources;
	int used_current_file;
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
	case '~':
	case '<':
	case '{':
	case '}':
	case '#':
		return 0;
	default:
		return 1;
	}
}

static int helper_quoted_word_allowed(unsigned char c) {
	return c >= 0x20 && c < 0x7f;
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
	tokens[*count].unquoted_glob = 0;
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

static int helper_token_can_end(helper_token *tokens, int count) {
	if (tokens == NULL || count <= 0) {
		return 0;
	}
	switch (tokens[count - 1].kind) {
	case HELPER_TOKEN_WORD:
	case HELPER_TOKEN_RPAREN:
	case HELPER_TOKEN_REDIR_NULL:
		return 1;
	default:
		return 0;
	}
}

static int helper_tokenize(const char *command, helper_token *tokens, int *count) {
	if (command == NULL || tokens == NULL || count == NULL) {
		return 0;
	}
	*count = 0;
	const unsigned char *p = (const unsigned char *)command;
	while (*p != '\0') {
		while (*p == ' ' || *p == '\t' || *p == '\n') {
			if (*p == '\n' && helper_token_can_end(tokens, *count)) {
				if (!helper_token_add(tokens, count, HELPER_TOKEN_SEMI, NULL, 0, 0)) {
					return 0;
				}
				p++;
				while (*p == ' ' || *p == '\t' || *p == '\n') {
					p++;
				}
				break;
			}
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
		int unquoted_glob = 0;
		while (!helper_word_meta(*p)) {
			if (*p == '\'' || *p == '"') {
				unsigned char quote = *p++;
				while (*p != '\0' && *p != quote) {
					if (!helper_quoted_word_allowed(*p) || len + 1 >= sizeof(word)) {
						return 0;
					}
					if (quote == '"') {
						if (*p == '$' || *p == '`' || *p == '!') {
							return 0;
						}
						if (*p == '\\') {
							if (p[1] == '\0' || p[1] == '$' || p[1] == '`' || p[1] == '\n') {
								return 0;
							}
							if (p[1] == '"' || p[1] == '\\') {
								word[len++] = (char)p[1];
								p += 2;
								continue;
							}
						}
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
			if (*p == '*' || *p == '?' || *p == '[' || *p == ']') {
				unquoted_glob = 1;
			}
			word[len++] = (char)*p++;
		}
		int token_index = *count;
		if (len == 0 || !helper_token_add(tokens, count, HELPER_TOKEN_WORD, word, len, 0)) {
			return 0;
		}
		tokens[token_index].unquoted_glob = unquoted_glob;
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

static void helper_mark_stderr_discarded(helper_plan *plan, int idx) {
	if (plan == NULL || idx < 0 || idx >= plan->count) {
		return;
	}
	helper_node *node = &plan->nodes[idx];
	node->stderr_discarded = 1;
	if (node->left >= 0) {
		helper_mark_stderr_discarded(plan, node->left);
	}
	if (node->right >= 0) {
		helper_mark_stderr_discarded(plan, node->right);
	}
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
			size_t word_len = strnlen(tok->text, HELPER_SHELL_WORD_BUF - 1);
			memmove(p->plan.nodes[node].argv[p->plan.nodes[node].argc], tok->text, word_len + 1);
			if (tok->unquoted_glob) {
				p->plan.nodes[node].shell_glob_mask |= UINT64_C(1) << p->plan.nodes[node].argc;
			}
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
		if (tok->fd == 2) {
			helper_mark_stderr_discarded(&p->plan, node);
		}
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

static int helper_note_replay(helper_result *res, const char *store_root, long long native_wall_ms,
	                          long long replay_start_ns, int current_file) {
	if (res == NULL || store_root == NULL || store_root[0] == '\0') {
		return 0;
	}
	if (res->replay_store_root[0] == '\0') {
		snprintf(res->replay_store_root, sizeof(res->replay_store_root), "%s", store_root);
	} else if (strcmp(res->replay_store_root, store_root) != 0) {
		return 0;
	}
	if (native_wall_ms > 0 && res->native_wall_ms <= LLONG_MAX - native_wall_ms) {
		res->native_wall_ms += native_wall_ms;
	}
	if (res->replay_start_ns == 0 || (replay_start_ns > 0 && replay_start_ns < res->replay_start_ns)) {
		res->replay_start_ns = replay_start_ns;
	}
	res->replay_sources++;
	res->used_current_file = res->used_current_file || current_file;
	return 1;
}

static int helper_merge_replay(helper_result *dst, const helper_result *src) {
	if (src == NULL || src->replay_sources == 0) {
		return 1;
	}
	if (dst == NULL || src->replay_store_root[0] == '\0') {
		return 0;
	}
	if (dst->replay_store_root[0] == '\0') {
		snprintf(dst->replay_store_root, sizeof(dst->replay_store_root), "%s", src->replay_store_root);
	} else if (strcmp(dst->replay_store_root, src->replay_store_root) != 0) {
		return 0;
	}
	if (src->native_wall_ms > 0 && dst->native_wall_ms <= LLONG_MAX - src->native_wall_ms) {
		dst->native_wall_ms += src->native_wall_ms;
	}
	if (dst->replay_start_ns == 0 || (src->replay_start_ns > 0 && src->replay_start_ns < dst->replay_start_ns)) {
		dst->replay_start_ns = src->replay_start_ns;
	}
	dst->replay_sources += src->replay_sources;
	dst->used_current_file = dst->used_current_file || src->used_current_file;
	return 1;
}

static int helper_copy_bytes(byte_buf *dst, const unsigned char *data, uint32_t len) {
	return len == 0 || bytes_append(dst, data, len);
}

static int helper_append_result(byte_buf *dst, const byte_buf *src) {
	return src == NULL || src->len == 0 || bytes_append(dst, src->data, src->len);
}

static int helper_append_line_selection(byte_buf *out, const unsigned char *content, uint32_t len,
                                        const line_selection *selection, size_t max_output) {
	if (selection == NULL || selection->count <= 0) {
		return 0;
	}
	int line = 1;
	int max_end = line_selection_max_end(selection);
	uint32_t offset = 0;
	while (offset < len && line <= max_end) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		uint32_t write_end = line_end;
		if (line_end < len && content[line_end] == '\n') {
			write_end = line_end + 1;
		}
		int matches = line_selection_match_count(selection, line);
		for (int match = 0; match < matches; match++) {
			size_t line_len = write_end - offset;
			if ((max_output > 0 && (out->len > max_output || line_len > max_output - out->len)) ||
			    !helper_copy_bytes(out, content + offset, (uint32_t)line_len)) {
				return 0;
			}
		}
		line++;
		offset = write_end;
	}
	return 1;
}

static int helper_append_sed_range(byte_buf *out, const unsigned char *content, uint32_t len, int start, int end) {
	line_selection selection = {0};
	selection.ranges[0].start = start;
	selection.ranges[0].end = end;
	selection.count = 1;
	return helper_append_line_selection(out, content, len, &selection, MAX_COMPOSED_INTERMEDIATE_BYTES);
}

static int helper_append_tail_lines(byte_buf *out, const unsigned char *content, uint32_t len, int count) {
	if (count == 0 || len == 0) {
		return 1;
	}
	int total = count_lines(content, len);
	int start = total - count + 1;
	if (start < 1) {
		start = 1;
	}
	return helper_append_sed_range(out, content, len, start, total);
}

static int helper_is_default_nl_delimiter(const unsigned char *line, size_t len) {
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

static int helper_append_nl_selection(byte_buf *out, const unsigned char *content, uint32_t len,
                                      const line_selection *selection, size_t max_output) {
	if (selection == NULL || selection->count <= 0) {
		return 0;
	}
	uint32_t offset = 0;
	while (offset < len) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		if (helper_is_default_nl_delimiter(content + offset, line_end - offset)) {
			return 0;
		}
		offset = line_end < len ? line_end + 1 : line_end;
	}
	int line = 1;
	int max_end = line_selection_max_end(selection);
	offset = 0;
	while (offset < len && line <= max_end) {
		uint32_t line_end = offset;
		while (line_end < len && content[line_end] != '\n') {
			line_end++;
		}
		uint32_t write_end = line_end < len ? line_end + 1 : line_end;
		int matches = line_selection_match_count(selection, line);
		for (int match = 0; match < matches; match++) {
			char prefix[32];
			int prefix_len = snprintf(prefix, sizeof(prefix), "%6d\t", line);
			if (prefix_len <= 0 || prefix_len >= (int)sizeof(prefix) ||
			    !bytes_append(out, (const unsigned char *)prefix, (size_t)prefix_len) ||
			    !helper_copy_bytes(out, content + offset, write_end - offset)) {
				return 0;
			}
#if defined(__linux__) || defined(SQUIRE_GNU_NL_TERMINATOR)
			if (write_end == len && (write_end == offset || content[write_end - 1] != '\n') &&
			    !bytes_append_byte(out, '\n')) {
				return 0;
			}
#endif
			if (out->len > max_output) {
				return 0;
			}
		}
		offset = write_end;
		line++;
	}
	return 1;
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
		if (out->len > MAX_FILE_OUTPUT_BYTES) {
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

static int helper_parse_line_count_arg(const helper_node *node, int *count) {
	if (node->argc == 3 && strcmp(node->argv[1], "-n") == 0) {
		char *end = NULL;
		errno = 0;
		long parsed = strtol(node->argv[2], &end, 10);
		if (errno != 0 || end == node->argv[2] || *end != '\0' || parsed < 0 || parsed > 100000) {
			return 0;
		}
		*count = (int)parsed;
		return 1;
	}
	if (node->argc == 2 && strncmp(node->argv[1], "-n", 2) == 0 && node->argv[1][2] != '\0') {
		char *end = NULL;
		errno = 0;
		long parsed = strtol(node->argv[1] + 2, &end, 10);
		if (errno != 0 || end == node->argv[1] + 2 || *end != '\0' || parsed < 0 || parsed > 100000) {
			return 0;
		}
		*count = (int)parsed;
		return 1;
	}
	if (node->argc == 2 && node->argv[1][0] == '-' && node->argv[1][1] != '\0') {
		char *end = NULL;
		errno = 0;
		long parsed = strtol(node->argv[1] + 1, &end, 10);
		if (errno != 0 || end == node->argv[1] + 1 || *end != '\0' || parsed < 0 || parsed > 100000) {
			return 0;
		}
		*count = (int)parsed;
		return 1;
	}
	return 0;
}

static int helper_parse_stdin_sed(const helper_node *node, line_selection *selection) {
	return node != NULL && node->argc == 3 && strcmp(node->argv[1], "-n") == 0 &&
	       parse_sed_print_selection(node->argv[2], selection);
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
	if (strcmp(name, "head") == 0 && helper_parse_line_count_arg(node, &count)) {
		out->exit_code = 0;
		return helper_append_sed_range(&out->stdout_buf, input->data, (uint32_t)input->len, 1, count);
	}
	if (strcmp(name, "tail") == 0 && helper_parse_line_count_arg(node, &count)) {
		out->exit_code = 0;
		return helper_append_tail_lines(&out->stdout_buf, input->data, (uint32_t)input->len, count);
	}
	line_selection selection;
	if (strcmp(name, "sed") == 0 && helper_parse_stdin_sed(node, &selection)) {
		out->exit_code = 0;
		return helper_append_line_selection(&out->stdout_buf, input->data, (uint32_t)input->len,
		                                    &selection, MAX_COMPOSED_INTERMEDIATE_BYTES);
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
	       strcmp(tool, "pwd") == 0 ||
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
	       strcmp(tool, "tail") == 0 ||
	       strcmp(tool, "nl") == 0;
}

static int helper_apply_warm_file(const warm_file_operation *operation, const unsigned char *content,
								  uint32_t content_len, helper_result *out) {
	if (operation == NULL) {
		return 0;
	}
	int ok = 0;
	if (operation->kind == WARM_FILE_OPERATION_CAT) {
		ok = helper_copy_bytes(&out->stdout_buf, content, content_len);
		out->exit_code = 0;
	} else if (operation->kind == WARM_FILE_OPERATION_SED) {
		ok = helper_append_line_selection(&out->stdout_buf, content, content_len,
		                                  &operation->selection, MAX_FILE_OUTPUT_BYTES);
		out->exit_code = 0;
	} else if (operation->kind == WARM_FILE_OPERATION_HEAD) {
		line_selection selection = {0};
		selection.ranges[0].start = 1;
		selection.ranges[0].end = operation->line_count;
		selection.count = 1;
		ok = helper_append_line_selection(&out->stdout_buf, content, content_len, &selection, MAX_FILE_OUTPUT_BYTES);
		out->exit_code = 0;
	} else if (operation->kind == WARM_FILE_OPERATION_TAIL) {
		ok = helper_append_tail_lines(&out->stdout_buf, content, content_len, operation->line_count) &&
		     out->stdout_buf.len <= MAX_FILE_OUTPUT_BYTES;
		out->exit_code = 0;
	} else if (operation->kind == WARM_FILE_OPERATION_NL) {
		line_selection selection = {0};
		selection.ranges[0].start = 1;
		selection.ranges[0].end = INT_MAX;
		selection.count = 1;
		ok = helper_append_nl_selection(&out->stdout_buf, content, content_len, &selection, MAX_FILE_OUTPUT_BYTES);
		out->exit_code = 0;
	} else if (operation->kind == WARM_FILE_OPERATION_GREP) {
		int matched = 0;
		ok = helper_append_fixed_grep(&out->stdout_buf, content, content_len,
		                              operation->pattern, operation->quiet, &matched);
		out->exit_code = matched ? 0 : 1;
	} else if (operation->kind == WARM_FILE_OPERATION_RG) {
		int matched = 0;
		ok = helper_append_fixed_rg(&out->stdout_buf, content, content_len, operation->pattern,
		                            operation->quiet, operation->line_number, &matched);
		out->exit_code = matched ? 0 : 1;
	}
	if (!ok) {
		helper_result_free(out);
		memset(out, 0, sizeof(*out));
	}
	return ok;
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
	char key[HASH_HEX], epoch[256], path[PATH_BUF];
	warm_file_operation operation;
	if (!warm_file_proof(&inv, key, epoch, path, &operation, NULL, NULL, NULL)) {
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
	mapped_snapshot snap = {0};
	if (map_snapshot(store_root, &snap) &&
	    snapshot_find(&snap, command_hash, epoch, HOT_KIND_WARM_FILE, &content, &content_len, &err, &err_len, &exit_code, &native_wall_ms) &&
	    err_len == 0 && exit_code == 0) {
		helper_result prepared = {0};
		int ok = helper_apply_warm_file(&operation, content, content_len, &prepared);
		unmap_snapshot(&snap);
		if (!ok) {
			return 0;
		}
		if (!helper_note_replay(&prepared, store_root, (long long)native_wall_ms, replay_start_ns, 0)) {
			helper_result_free(&prepared);
			return 0;
		}
		*out = prepared;
		mmap_trace_path("shell-ir-helper-warm-file-hit", inv.argv[0]);
		return 1;
	}
	unmap_snapshot(&snap);

	unsigned char *current_content = NULL;
	size_t current_len = 0;
	if (!warm_file_proof(&inv, key, epoch, path, &operation, NULL, &current_content, &current_len)) {
		return 0;
	}
	if (current_len > UINT32_MAX) {
		free(current_content);
		return 0;
	}
	helper_result current = {0};
	int ok = helper_apply_warm_file(&operation, current_content, (uint32_t)current_len, &current);
	free(current_content);
	if (!ok) {
		return 0;
	}
	*out = current;
	mmap_trace_path("shell-ir-helper-current-file-hit", inv.argv[0]);
	if (!helper_note_replay(out, store_root, 0, replay_start_ns, 1)) {
		helper_result_free(out);
		return 0;
	}
	return 1;
}

static int helper_prepare_warm_file(int argc, char **argv, helper_result *out, long long replay_start_ns) {
	return helper_prepare_warm_file_at_cwd(NULL, argc, argv, out, replay_start_ns);
}

#define HELPER_RG_MAX_PATHS (MAX_ARGC - 2)
#define HELPER_RG_MAX_GLOBS 8
#define HELPER_RG_CORPUS_HEADER_BYTES 48
#define HELPER_RG_CORPUS_RECORD_BYTES 24
#define HELPER_RG_CORPUS_LINE_BYTES 24
#define HELPER_RG_CORPUS_MAX_FILES 10000
#define HELPER_RG_CORPUS_FOLD_UNSAFE UINT32_C(0x80000000)
#define HELPER_RG_MAX_SEED_MASKS 32
#define HELPER_RG_MAX_SEED_BYTES 64

typedef struct {
	const char *pattern;
	const char *paths[HELPER_RG_MAX_PATHS];
	const char *globs[HELPER_RG_MAX_GLOBS];
	int path_count;
	int glob_count;
	int fixed;
	int line_number;
	int ignore_case;
	int smart_case;
	int hidden;
	int quiet;
	int files_with_matches;
	int with_filename;
	int no_filename;
	int implicit_path;
	int allow_missing_paths;
} helper_repo_rg_query;

typedef struct {
	const unsigned char *data;
	uint32_t len;
	uint32_t file_count;
	uint32_t default_count;
	uint32_t hidden_count;
	uint32_t records_offset;
	uint32_t default_offset;
	uint32_t hidden_offset;
	uint32_t line_records_offset;
	uint32_t line_count;
	uint32_t payload_offset;
} helper_repo_rg_corpus;

typedef struct {
	const unsigned char *path;
	uint32_t path_len;
	const unsigned char *content;
	uint32_t content_len;
	uint32_t line_start;
	uint32_t line_count;
	int fold_unsafe;
} helper_repo_rg_file;

typedef struct {
	uint64_t mask;
	uint32_t literal_len;
	unsigned char literal[HELPER_RG_MAX_SEED_BYTES];
} helper_repo_rg_seed;

static int helper_repo_rg_text_safe(const char *value, size_t max_len) {
	if (value == NULL || value[0] == '\0' || strnlen(value, max_len + 1) > max_len) {
		return 0;
	}
	for (const unsigned char *p = (const unsigned char *)value; *p != '\0'; p++) {
		if (*p < 0x20 || *p >= 0x7f) {
			return 0;
		}
	}
	return 1;
}

#define HELPER_RG_TRANSLATED_REGEX_BYTES 8192

/* Rust regex defines \s as Unicode White_Space. Spell that set out instead
 * of relying on the host locale's POSIX character classes. Newlines are not
 * present in the per-line buffers evaluated below. */
static const char helper_repo_rg_unicode_space[] =
	"([\t\v\f\r ]|"
	"\xC2\x85|\xC2\xA0|\xE1\x9A\x80|"
	"\xE2\x80\x80|\xE2\x80\x81|\xE2\x80\x82|\xE2\x80\x83|"
	"\xE2\x80\x84|\xE2\x80\x85|\xE2\x80\x86|\xE2\x80\x87|"
	"\xE2\x80\x88|\xE2\x80\x89|\xE2\x80\x8A|"
	"\xE2\x80\xA8|\xE2\x80\xA9|\xE2\x80\xAF|"
	"\xE2\x81\x9F|\xE3\x80\x80)";

static int helper_repo_rg_translate_regex(const char *pattern, char *out, size_t out_cap) {
	if (out == NULL || out_cap == 0 || !helper_repo_rg_text_safe(pattern, 2048) || strstr(pattern, "(?") != NULL) {
		return 0;
	}
	size_t used = 0;
	int in_class = 0;
	for (size_t i = 0; pattern[i] != '\0'; i++) {
		char current = pattern[i];
		if (current == '[') {
			in_class = 1;
		} else if (current == ']' && in_class) {
			in_class = 0;
		}
		if (current != '\\') {
			if (used + 1 >= out_cap) {
				return 0;
			}
			out[used++] = current;
			continue;
		}
		char escaped = pattern[++i];
		if (escaped == '\0') {
			return 0;
		}
		if (isalnum((unsigned char)escaped)) {
			if (escaped != 's' || in_class) {
				return 0;
			}
			size_t expansion_len = strlen(helper_repo_rg_unicode_space);
			if (used + expansion_len >= out_cap) {
				return 0;
			}
			memcpy(out + used, helper_repo_rg_unicode_space, expansion_len);
			used += expansion_len;
			continue;
		}
		if (used + 2 >= out_cap) {
			return 0;
		}
		out[used++] = '\\';
		out[used++] = escaped;
	}
	out[used] = '\0';
	return 1;
}

static int helper_repo_rg_regex_supported(const char *pattern) {
	char translated[HELPER_RG_TRANSLATED_REGEX_BYTES];
	return helper_repo_rg_translate_regex(pattern, translated, sizeof(translated));
}

static int helper_repo_rg_parse(int argc, char **argv, helper_repo_rg_query *query) {
	if (query == NULL || argc < 2 || argc > MAX_ARGC || argv == NULL || strcmp(base_name(argv[0]), "rg") != 0) {
		return 0;
	}
	memset(query, 0, sizeof(*query));
	for (int i = 1; i < argc; i++) {
		const char *arg = argv[i];
		if (strcmp(arg, "-n") == 0 || strcmp(arg, "--line-number") == 0) {
			query->line_number = 1;
			continue;
		}
		if (strcmp(arg, "-F") == 0 || strcmp(arg, "--fixed-strings") == 0) {
			query->fixed = 1;
			continue;
		}
		if (strcmp(arg, "-i") == 0 || strcmp(arg, "--ignore-case") == 0) {
			query->ignore_case = 1;
			continue;
		}
		if (strcmp(arg, "-S") == 0 || strcmp(arg, "--smart-case") == 0) {
			query->smart_case = 1;
			continue;
		}
		if (strcmp(arg, "--hidden") == 0) {
			query->hidden = 1;
			continue;
		}
		if (strcmp(arg, "-q") == 0 || strcmp(arg, "--quiet") == 0) {
			query->quiet = 1;
			continue;
		}
		if (strcmp(arg, "-l") == 0 || strcmp(arg, "--files-with-matches") == 0) {
			query->files_with_matches = 1;
			continue;
		}
		if (strcmp(arg, "--with-filename") == 0) {
			query->with_filename = 1;
			continue;
		}
		if (strcmp(arg, "--no-filename") == 0) {
			query->no_filename = 1;
			continue;
		}
		if (strcmp(arg, "--no-heading") == 0) {
			continue;
		}
		if (strcmp(arg, "-g") == 0 || strcmp(arg, "--glob") == 0) {
			if (++i >= argc || query->glob_count >= HELPER_RG_MAX_GLOBS || !helper_repo_rg_text_safe(argv[i], 1024)) {
				return 0;
			}
			query->globs[query->glob_count++] = argv[i];
			continue;
		}
		if (strncmp(arg, "--glob=", 7) == 0 || (strncmp(arg, "-g", 2) == 0 && arg[2] != '\0')) {
			const char *glob = strncmp(arg, "--glob=", 7) == 0 ? arg + 7 : arg + 2;
			if (query->glob_count >= HELPER_RG_MAX_GLOBS || !helper_repo_rg_text_safe(glob, 1024)) {
				return 0;
			}
			query->globs[query->glob_count++] = glob;
			continue;
		}
		if (arg[0] == '-') {
			return 0;
		}
		if (query->pattern == NULL) {
			query->pattern = arg;
			continue;
		}
		if (query->path_count >= HELPER_RG_MAX_PATHS || (strcmp(arg, ".") != 0 && !safe_relative_inspection_path_arg(arg))) {
			return 0;
		}
		query->paths[query->path_count++] = arg;
	}
	if (query->pattern == NULL || !query->line_number || (query->with_filename && query->no_filename)) {
		return 0;
	}
	if ((!query->fixed && !helper_repo_rg_regex_supported(query->pattern)) ||
	    (query->fixed && !helper_repo_rg_text_safe(query->pattern, 2048))) {
		return 0;
	}
	if (query->path_count == 0) {
		query->implicit_path = 1;
		query->paths[query->path_count++] = ".";
	}
	if (query->hidden) {
		int excludes_git = 0;
		for (int i = 0; i < query->glob_count; i++) {
			if (strcmp(query->globs[i], "!**/.git/**") == 0 ||
			    strcmp(query->globs[i], "!.git/**") == 0 ||
			    strcmp(query->globs[i], "!/.git/**") == 0) {
				excludes_git = 1;
			}
		}
		if (!excludes_git) {
			return 0;
		}
	}
	return 1;
}

static int helper_shell_glob_arg_safe(const char *arg) {
	if (arg == NULL || arg[0] == '\0' || arg[0] == '/' || arg[0] == '-' || strlen(arg) >= PATH_BUF ||
	    strpbrk(arg, "*?[") == NULL) {
		return 0;
	}
	const char *part = arg;
	while (*part != '\0') {
		const char *slash = strchr(part, '/');
		size_t len = slash == NULL ? strlen(part) : (size_t)(slash - part);
		if (len == 2 && part[0] == '.' && part[1] == '.') {
			return 0;
		}
		if (slash == NULL) {
			break;
		}
		part = slash + 1;
	}
	return 1;
}

static int helper_expand_shell_globs(const char *cwd, int argc, char **argv, uint64_t shell_glob_mask,
	                                 int *expanded_argc, char *expanded_argv[MAX_ARGC],
	                                 char expanded_storage[MAX_ARGC][PATH_BUF]) {
	if (cwd == NULL || cwd[0] != '/' || argc <= 0 || argc > MAX_ARGC || argv == NULL || expanded_argc == NULL) {
		return 0;
	}
	int outc = 0;
	size_t cwd_len = strlen(cwd);
	while (cwd_len > 1 && cwd[cwd_len - 1] == '/') {
		cwd_len--;
	}
	for (int i = 0; i < argc; i++) {
		if ((shell_glob_mask & (UINT64_C(1) << i)) == 0) {
			if (outc >= MAX_ARGC) {
				return 0;
			}
			expanded_argv[outc++] = argv[i];
			continue;
		}
		if (i == 0 || !helper_shell_glob_arg_safe(argv[i])) {
			return 0;
		}
		char pattern[PATH_BUF];
		if (!join_path(pattern, sizeof(pattern), cwd, argv[i])) {
			return 0;
		}
		glob_t matches;
		memset(&matches, 0, sizeof(matches));
		int rc = glob(pattern, GLOB_ERR, NULL, &matches);
		if (rc != 0 || matches.gl_pathc == 0) {
			globfree(&matches);
			return 0;
		}
		for (size_t j = 0; j < matches.gl_pathc; j++) {
			const char *match = matches.gl_pathv[j];
			if (outc >= MAX_ARGC || strncmp(match, cwd, cwd_len) != 0 || match[cwd_len] != '/') {
				globfree(&matches);
				return 0;
			}
			const char *rel = match + cwd_len + 1;
			int n = strncmp(argv[i], "./", 2) == 0
			        ? snprintf(expanded_storage[outc], PATH_BUF, "./%s", rel)
			        : snprintf(expanded_storage[outc], PATH_BUF, "%s", rel);
			if (n <= 0 || n >= PATH_BUF) {
				globfree(&matches);
				return 0;
			}
			expanded_argv[outc] = expanded_storage[outc];
			outc++;
		}
		globfree(&matches);
	}
	*expanded_argc = outc;
	return outc > 0;
}

static int helper_repo_rg_decode_corpus(const unsigned char *data, uint32_t len, helper_repo_rg_corpus *corpus) {
	static const unsigned char magic[8] = {'S', 'Q', 'R', 'G', 'C', '0', '0', '1'};
	if (data == NULL || corpus == NULL || len < HELPER_RG_CORPUS_HEADER_BYTES || len > MAX_REPO_SEARCH_CORPUS_BYTES ||
	    memcmp(data, magic, sizeof(magic)) != 0 || le32(data + 8) != 2) {
		return 0;
	}
	memset(corpus, 0, sizeof(*corpus));
	corpus->data = data;
	corpus->len = len;
	corpus->file_count = le32(data + 12);
	corpus->default_count = le32(data + 16);
	corpus->hidden_count = le32(data + 20);
	corpus->records_offset = le32(data + 24);
	corpus->default_offset = le32(data + 28);
	corpus->hidden_offset = le32(data + 32);
	corpus->payload_offset = le32(data + 36);
	uint32_t total_size = le32(data + 40);
	corpus->line_records_offset = le32(data + 44);
	if (corpus->file_count == 0 || corpus->file_count > HELPER_RG_CORPUS_MAX_FILES ||
	    corpus->default_count > corpus->file_count || corpus->hidden_count > corpus->file_count ||
	    corpus->records_offset != HELPER_RG_CORPUS_HEADER_BYTES || total_size != len) {
		return 0;
	}
	uint64_t records_end = (uint64_t)corpus->records_offset + (uint64_t)corpus->file_count * HELPER_RG_CORPUS_RECORD_BYTES;
	uint64_t default_end = (uint64_t)corpus->default_offset + (uint64_t)corpus->default_count * 4;
	uint64_t hidden_end = (uint64_t)corpus->hidden_offset + (uint64_t)corpus->hidden_count * 4;
	if (corpus->line_records_offset < hidden_end || corpus->payload_offset < corpus->line_records_offset ||
	    (corpus->payload_offset - corpus->line_records_offset) % HELPER_RG_CORPUS_LINE_BYTES != 0) {
		return 0;
	}
	corpus->line_count = (corpus->payload_offset - corpus->line_records_offset) / HELPER_RG_CORPUS_LINE_BYTES;
	return records_end == corpus->default_offset && default_end == corpus->hidden_offset &&
	       hidden_end == corpus->line_records_offset && corpus->payload_offset <= len;
}

static int helper_repo_rg_file_at(const helper_repo_rg_corpus *corpus, uint32_t index, helper_repo_rg_file *file) {
	if (corpus == NULL || file == NULL || index >= corpus->file_count) {
		return 0;
	}
	const unsigned char *record = corpus->data + corpus->records_offset + index * HELPER_RG_CORPUS_RECORD_BYTES;
	uint32_t path_offset = le32(record);
	uint32_t path_len = le32(record + 4);
	uint32_t content_offset = le32(record + 8);
	uint32_t content_len = le32(record + 12);
	uint32_t line_start = le32(record + 16);
	uint32_t encoded_line_count = le32(record + 20);
	uint32_t line_count = encoded_line_count & ~HELPER_RG_CORPUS_FOLD_UNSAFE;
	if (path_len == 0 || path_len >= PATH_BUF || path_offset < corpus->payload_offset || content_offset < corpus->payload_offset ||
	    path_offset > corpus->len || content_offset > corpus->len || path_len > corpus->len - path_offset ||
	    content_len > corpus->len - content_offset || line_start > corpus->line_count ||
	    line_count > corpus->line_count - line_start) {
		return 0;
	}
	file->path = corpus->data + path_offset;
	file->path_len = path_len;
	file->content = corpus->data + content_offset;
	file->content_len = content_len;
	file->line_start = line_start;
	file->line_count = line_count;
	file->fold_unsafe = (encoded_line_count & HELPER_RG_CORPUS_FOLD_UNSAFE) != 0;
	return memchr(file->path, '\0', path_len) == NULL;
}

static uint64_t helper_repo_rg_bloom_bits(const unsigned char *value, size_t len) {
	uint64_t hash = UINT64_C(1469598103934665603);
	hash ^= (uint64_t)len;
	hash *= UINT64_C(1099511628211);
	for (size_t i = 0; i < len; i++) {
		hash ^= value[i];
		hash *= UINT64_C(1099511628211);
	}
	return (UINT64_C(1) << (hash & 63)) | (UINT64_C(1) << ((hash >> 6) & 63));
}

static uint64_t helper_repo_rg_literal_mask(const unsigned char *literal, size_t len, int fold_ascii) {
	uint64_t mask = 0;
	for (size_t width = 1; width <= 3; width++) {
		for (size_t start = 0; start + width <= len; start++) {
			unsigned char folded[3];
			for (size_t i = 0; i < width; i++) {
				unsigned char value = literal[start + i];
				if (fold_ascii && value >= 'A' && value <= 'Z') {
					value = (unsigned char)(value + ('a' - 'A'));
				}
				folded[i] = value;
			}
			mask |= helper_repo_rg_bloom_bits(folded, width);
		}
	}
	return mask;
}

static int helper_repo_rg_set_seed(helper_repo_rg_seed *seed, const unsigned char *literal,
	                               size_t len, int fold_ascii) {
	if (seed == NULL || literal == NULL || len == 0) {
		return 0;
	}
	size_t kept = len < HELPER_RG_MAX_SEED_BYTES ? len : HELPER_RG_MAX_SEED_BYTES;
	for (size_t i = 0; i < kept; i++) {
		unsigned char value = literal[i];
		if (fold_ascii && value >= 'A' && value <= 'Z') {
			value = (unsigned char)(value + ('a' - 'A'));
		}
		seed->literal[i] = value;
	}
	seed->literal_len = (uint32_t)kept;
	seed->mask = helper_repo_rg_literal_mask(seed->literal, kept, 0);
	return seed->mask != 0;
}

static int helper_repo_rg_seed_matches(const unsigned char *line, size_t line_len,
	                                    const helper_repo_rg_seed *seed, int ignore_case) {
	if (line == NULL || seed == NULL || seed->literal_len == 0 || seed->literal_len > line_len) {
		return 0;
	}
	size_t literal_len = seed->literal_len;
	for (size_t start = 0; start + literal_len <= line_len; start++) {
		unsigned char first = line[start];
		if (ignore_case && first >= 'A' && first <= 'Z') {
			first = (unsigned char)(first + ('a' - 'A'));
		}
		if (first != seed->literal[0]) {
			continue;
		}
		size_t index = 1;
		for (; index < literal_len; index++) {
			unsigned char value = line[start + index];
			if (ignore_case && value >= 'A' && value <= 'Z') {
				value = (unsigned char)(value + ('a' - 'A'));
			}
			if (value != seed->literal[index]) {
				break;
			}
		}
		if (index == literal_len) {
			return 1;
		}
	}
	return 0;
}

static void helper_repo_rg_best_literal(unsigned char best[2048], size_t *best_len,
	                                    unsigned char current[2048], size_t *current_len) {
	if (*current_len > *best_len) {
		memcpy(best, current, *current_len);
		*best_len = *current_len;
	}
	*current_len = 0;
}

/* Extract one provably required top-level literal from every alternative.
 * This is only a rejection index: Bloom false positives still reach regexec,
 * while any construct whose required literal is unclear disables the index. */
static int helper_repo_rg_seed_masks(const helper_repo_rg_query *query, int ignore_case,
	                                 helper_repo_rg_seed seeds[HELPER_RG_MAX_SEED_MASKS],
	                                 int *seed_count) {
	if (query == NULL || query->pattern == NULL || seed_count == NULL) {
		return 0;
	}
	if (query->fixed) {
		size_t len = strlen(query->pattern);
		*seed_count = helper_repo_rg_set_seed(&seeds[0], (const unsigned char *)query->pattern,
		                                      len, ignore_case) ? 1 : 0;
		return *seed_count == 1;
	}

	unsigned char current[2048], best[2048];
	size_t current_len = 0;
	size_t best_len = 0;
	int depth = 0;
	int in_class = 0;
	int count = 0;
	const unsigned char *pattern = (const unsigned char *)query->pattern;
	for (size_t i = 0;; i++) {
		unsigned char value = pattern[i];
		if (value == '\0' || (value == '|' && depth == 0 && !in_class)) {
			helper_repo_rg_best_literal(best, &best_len, current, &current_len);
			if (best_len == 0 || count >= HELPER_RG_MAX_SEED_MASKS) {
				return 0;
			}
			if (!helper_repo_rg_set_seed(&seeds[count], best, best_len, ignore_case)) {
				return 0;
			}
			count++;
			best_len = 0;
			if (value == '\0') {
				break;
			}
			continue;
		}
		if (value == '\\') {
			unsigned char escaped = pattern[++i];
			if (escaped == '\0') {
				return 0;
			}
			if (depth == 0 && !in_class && !isalnum(escaped)) {
				if (current_len >= sizeof(current)) {
					return 0;
				}
				current[current_len++] = escaped;
			} else {
				helper_repo_rg_best_literal(best, &best_len, current, &current_len);
			}
			continue;
		}
		if (in_class) {
			if (value == ']') {
				in_class = 0;
			}
			continue;
		}
		if (value == '[') {
			helper_repo_rg_best_literal(best, &best_len, current, &current_len);
			in_class = 1;
			continue;
		}
		if (value == '(') {
			helper_repo_rg_best_literal(best, &best_len, current, &current_len);
			depth++;
			continue;
		}
		if (value == ')') {
			if (depth <= 0) {
				return 0;
			}
			depth--;
			continue;
		}
		if (depth > 0) {
			continue;
		}
		if (value == '*' || value == '?') {
			if (current_len > 0) {
				current_len--;
			}
			helper_repo_rg_best_literal(best, &best_len, current, &current_len);
			continue;
		}
		if (value == '{') {
			if (current_len > 0) {
				current_len--;
			}
			helper_repo_rg_best_literal(best, &best_len, current, &current_len);
			const unsigned char *close = (const unsigned char *)strchr((const char *)pattern + i + 1, '}');
			if (close == NULL) {
				return 0;
			}
			i = (size_t)(close - pattern);
			continue;
		}
		if (value == '}') {
			return 0;
		}
		if (value == '.' || value == '^' || value == '$' || value == '+') {
			helper_repo_rg_best_literal(best, &best_len, current, &current_len);
			continue;
		}
		if (current_len >= sizeof(current)) {
			return 0;
		}
		current[current_len++] = value;
	}
	if (depth != 0 || in_class || count == 0) {
		return 0;
	}
	*seed_count = count;
	return 1;
}

static int helper_repo_rg_line_at(const helper_repo_rg_corpus *corpus, const helper_repo_rg_file *file,
	                              uint32_t line_index, const unsigned char **line, uint32_t *line_len,
	                              uint64_t *bloom, uint64_t *folded_bloom) {
	if (corpus == NULL || file == NULL || line == NULL || line_len == NULL || bloom == NULL || folded_bloom == NULL ||
	    line_index >= file->line_count) {
		return 0;
	}
	uint32_t global_index = file->line_start + line_index;
	if (global_index >= corpus->line_count) {
		return 0;
	}
	const unsigned char *record = corpus->data + corpus->line_records_offset +
	                              global_index * HELPER_RG_CORPUS_LINE_BYTES;
	uint32_t offset = le32(record);
	uint32_t length = le32(record + 4);
	if (offset > file->content_len || length > file->content_len - offset) {
		return 0;
	}
	*line = file->content + offset;
	*line_len = length;
	*bloom = le64(record + 8);
	*folded_bloom = le64(record + 16);
	return 1;
}

static int helper_repo_rg_fnmatch_braced(const char *pattern, const char *path) {
	const char *open = strchr(pattern, '{');
	const char *close = open == NULL ? NULL : strchr(open + 1, '}');
	if (open == NULL || close == NULL) {
		if (fnmatch(pattern, path, 0) == 0) {
			return 1;
		}
		return strncmp(pattern, "**/", 3) == 0 && fnmatch(pattern + 3, path, 0) == 0;
	}
	if (strchr(open + 1, '{') != NULL || strchr(close + 1, '}') != NULL) {
		return 0;
	}
	const char *item = open + 1;
	while (item <= close) {
		const char *comma = memchr(item, ',', (size_t)(close - item));
		const char *end = comma == NULL ? close : comma;
		char expanded[1024];
		size_t prefix = (size_t)(open - pattern);
		size_t middle = (size_t)(end - item);
		size_t suffix = strlen(close + 1);
		if (prefix + middle + suffix + 1 > sizeof(expanded)) {
			return 0;
		}
		memcpy(expanded, pattern, prefix);
		memcpy(expanded + prefix, item, middle);
		memcpy(expanded + prefix + middle, close + 1, suffix + 1);
		if (fnmatch(expanded, path, 0) == 0 ||
		    (strncmp(expanded, "**/", 3) == 0 && fnmatch(expanded + 3, path, 0) == 0)) {
			return 1;
		}
		if (comma == NULL) {
			break;
		}
		item = comma + 1;
	}
	return 0;
}

static int helper_repo_rg_component_match(const char *pattern, const char *path) {
	if (helper_repo_rg_fnmatch_braced(pattern, path)) {
		return 1;
	}
	const char *part = path;
	for (;;) {
		const char *slash = strchr(part, '/');
		size_t len = slash == NULL ? strlen(part) : (size_t)(slash - part);
		if (len > 0 && len < PATH_BUF) {
			char component[PATH_BUF];
			memcpy(component, part, len);
			component[len] = '\0';
			if (helper_repo_rg_fnmatch_braced(pattern, component)) {
				return 1;
			}
		}
		if (slash == NULL) {
			return 0;
		}
		part = slash + 1;
	}
}

static int helper_repo_rg_globs_match(const helper_repo_rg_query *query, const char *path) {
	int has_positive = 0;
	int positive_match = 0;
	for (int i = 0; i < query->glob_count; i++) {
		const char *glob = query->globs[i];
		int negative = glob[0] == '!';
		const char *pattern = negative ? glob + 1 : glob;
		if (strcmp(pattern, "**/.git/**") == 0 || strcmp(pattern, ".git/**") == 0 ||
		    strcmp(pattern, "/.git/**") == 0) {
			continue;
		}
		int matched;
		if (strchr(pattern, '/') == NULL) {
			const char *base = base_name(path);
			matched = helper_repo_rg_fnmatch_braced(pattern, base);
			if (negative && !matched) {
				matched = helper_repo_rg_component_match(pattern, path);
			}
		} else {
			matched = helper_repo_rg_fnmatch_braced(pattern, path);
		}
		if (negative && matched) {
			return 0;
		}
		if (!negative) {
			has_positive = 1;
			positive_match |= matched;
		}
	}
	return !has_positive || positive_match;
}

static int helper_repo_rg_path_kind(const helper_repo_rg_corpus *corpus, const uint32_t *order,
	                                uint32_t count, const char *path, int *is_directory) {
	if (strcmp(path, ".") == 0) {
		*is_directory = 1;
		return 1;
	}
	*is_directory = 0;
	size_t path_len = strlen(path);
	for (uint32_t i = 0; i < count; i++) {
		uint32_t index = le32((const unsigned char *)&order[i]);
		helper_repo_rg_file file;
		if (!helper_repo_rg_file_at(corpus, index, &file)) {
			return -1;
		}
		if (file.path_len == path_len && memcmp(file.path, path, path_len) == 0) {
			return 1;
		}
		if (path_len < file.path_len && memcmp(file.path, path, path_len) == 0 && file.path[path_len] == '/') {
			*is_directory = 1;
			return 1;
		}
	}
	return 0;
}

static int helper_repo_rg_path_selects(const char *root, const char *path) {
	if (strcmp(root, ".") == 0) {
		return 1;
	}
	size_t n = strlen(root);
	return strcmp(root, path) == 0 || (strncmp(path, root, n) == 0 && path[n] == '/');
}

static int helper_repo_rg_pattern_has_upper(const char *pattern) {
	for (const unsigned char *p = (const unsigned char *)pattern; *p != '\0'; p++) {
		if (isupper(*p)) {
			return 1;
		}
	}
	return 0;
}

static int helper_repo_rg_fixed_match(const unsigned char *line, size_t line_len, const char *pattern, int ignore_case) {
	size_t pattern_len = strlen(pattern);
	if (pattern_len == 0 || pattern_len > line_len) {
		return 0;
	}
	for (size_t i = 0; i + pattern_len <= line_len; i++) {
		int equal = 1;
		for (size_t j = 0; j < pattern_len; j++) {
			unsigned char left = line[i + j];
			unsigned char right = (unsigned char)pattern[j];
			if (ignore_case) {
				left = (unsigned char)tolower(left);
				right = (unsigned char)tolower(right);
			}
			if (left != right) {
				equal = 0;
				break;
			}
		}
		if (equal) {
			return 1;
		}
	}
	return 0;
}

static int helper_repo_rg_append_path(byte_buf *out, const helper_repo_rg_query *query,
	                                  const char *root, const char *path) {
	if (strcmp(root, ".") == 0 && !query->implicit_path && !bytes_append_str(out, "./")) {
		return 0;
	}
	return bytes_append_str(out, path);
}

static int helper_repo_rg_emit_line(const helper_repo_rg_query *query, const char *root,
	                                const char *path, int show_filename,
	                                const unsigned char *line, size_t line_len,
	                                int line_number, byte_buf *out, int *matched_any) {
	*matched_any = 1;
	if (query->quiet) {
		return 1;
	}
	if (query->files_with_matches) {
		return helper_repo_rg_append_path(out, query, root, path) && bytes_append_byte(out, '\n');
	}
	if (show_filename && (!helper_repo_rg_append_path(out, query, root, path) || !bytes_append_byte(out, ':'))) {
		return 0;
	}
	if (query->line_number && (!helper_append_decimal_int(out, line_number) || !bytes_append_byte(out, ':'))) {
		return 0;
	}
	return helper_copy_bytes(out, line, line_len) && bytes_append_byte(out, '\n') &&
	       out->len <= MAX_FAST_OUTPUT_BYTES;
}

static int helper_repo_rg_eval_indexed_file(const helper_repo_rg_query *query,
	                                        const helper_repo_rg_corpus *corpus,
	                                        const helper_repo_rg_file *file,
	                                        const char *root, int show_filename,
	                                        regex_t *regex, int use_regex,
	                                        byte_buf *scratch, byte_buf *out,
	                                        int *matched_any,
	                                        const helper_repo_rg_seed *seeds,
	                                        int seed_count, int ignore_case) {
	if (memchr(file->content, '\0', file->content_len) != NULL) {
		return 1;
	}
	char path[PATH_BUF];
	if (file->path_len >= sizeof(path)) {
		return 0;
	}
	memcpy(path, file->path, file->path_len);
	path[file->path_len] = '\0';
	for (uint32_t index = 0; index < file->line_count; index++) {
		const unsigned char *line = NULL;
		uint32_t line_len = 0;
		uint64_t bloom = 0, folded_bloom = 0;
		if (!helper_repo_rg_line_at(corpus, file, index, &line, &line_len, &bloom, &folded_bloom)) {
			return 0;
		}
		uint64_t selected_bloom = ignore_case ? folded_bloom : bloom;
		int candidate = 0;
		for (int seed = 0; seed < seed_count; seed++) {
			if ((selected_bloom & seeds[seed].mask) == seeds[seed].mask &&
			    helper_repo_rg_seed_matches(line, line_len, &seeds[seed], ignore_case)) {
				candidate = 1;
				break;
			}
		}
		if (!candidate) {
			continue;
		}
		int matched = 0;
		if (!use_regex) {
			matched = helper_repo_rg_fixed_match(line, line_len, query->pattern, ignore_case);
		} else {
			scratch->len = 0;
			if (!bytes_append(scratch, line, line_len) || !bytes_append_byte(scratch, '\0')) {
				return 0;
			}
			int rc = regexec(regex, (const char *)scratch->data, 0, NULL, 0);
			if (rc != 0 && rc != REG_NOMATCH) {
				return 0;
			}
			matched = rc == 0;
		}
		if (!matched) {
			continue;
		}
		if (!helper_repo_rg_emit_line(query, root, path, show_filename, line, line_len,
		                              (int)index + 1, out, matched_any)) {
			return 0;
		}
		if (query->quiet || query->files_with_matches) {
			return 1;
		}
	}
	return 1;
}

static int helper_repo_rg_eval_file(const helper_repo_rg_query *query, const helper_repo_rg_file *file,
	                                const helper_repo_rg_corpus *corpus, const char *root,
	                                int show_filename, regex_t *regex, int use_regex,
	                                byte_buf *scratch, byte_buf *out, int *matched_any,
	                                const helper_repo_rg_seed *seeds, int seed_count,
	                                int ignore_case) {
	if (seed_count > 0) {
		return helper_repo_rg_eval_indexed_file(query, corpus, file, root, show_filename,
		                                        regex, use_regex, scratch, out, matched_any,
		                                        seeds, seed_count, ignore_case);
	}
	if (memchr(file->content, '\0', file->content_len) != NULL) {
		return 1;
	}
	char path[PATH_BUF];
	if (file->path_len >= sizeof(path)) {
		return 0;
	}
	memcpy(path, file->path, file->path_len);
	path[file->path_len] = '\0';
	uint32_t offset = 0;
	int line_number = 1;
	int file_matched = 0;
	while (offset < file->content_len) {
		uint32_t end = offset;
		while (end < file->content_len && file->content[end] != '\n') {
			end++;
		}
		size_t line_len = (size_t)(end - offset);
		int matched = 0;
		if (!use_regex) {
			matched = helper_repo_rg_fixed_match(file->content + offset, line_len, query->pattern, ignore_case);
		} else {
			scratch->len = 0;
			if (!bytes_append(scratch, file->content + offset, line_len) || !bytes_append_byte(scratch, '\0')) {
				return 0;
			}
			int rc = regexec(regex, (const char *)scratch->data, 0, NULL, 0);
			if (rc != 0 && rc != REG_NOMATCH) {
				return 0;
			}
			matched = rc == 0;
		}
		if (matched) {
			if (!helper_repo_rg_emit_line(query, root, path, show_filename,
			                              file->content + offset, line_len, line_number,
			                              out, matched_any)) {
				return 0;
			}
			file_matched = 1;
			if (query->quiet || query->files_with_matches) {
				return 1;
			}
		}
		offset = end < file->content_len ? end + 1 : end;
		line_number++;
	}
	(void)file_matched;
	return 1;
}

static int helper_repo_rg_evaluate(const helper_repo_rg_query *query, const helper_repo_rg_corpus *corpus, helper_result *out) {
	const unsigned char *order_bytes = corpus->data + (query->hidden ? corpus->hidden_offset : corpus->default_offset);
	uint32_t count = query->hidden ? corpus->hidden_count : corpus->default_count;
	const uint32_t *order = (const uint32_t *)order_bytes;
	int path_is_directory[HELPER_RG_MAX_PATHS] = {0};
	int path_exists[HELPER_RG_MAX_PATHS] = {0};
	int has_directory = 0;
	int missing_path = 0;
	for (int i = 0; i < query->path_count; i++) {
		int path_kind = helper_repo_rg_path_kind(corpus, order, count, query->paths[i], &path_is_directory[i]);
		if (path_kind < 0 || (path_kind == 0 && !query->allow_missing_paths)) {
			return 0;
		}
		path_exists[i] = path_kind == 1;
		missing_path |= path_kind == 0;
		has_directory |= path_is_directory[i];
	}
	int show_filename = query->with_filename || (!query->no_filename && (query->path_count > 1 || has_directory));
	int ignore_case = query->ignore_case || (query->smart_case && !helper_repo_rg_pattern_has_upper(query->pattern));
	/* The native Rust regex engine applies Unicode case folding. The compact
	 * index folds ASCII only, so files containing invalid UTF-8 or non-ASCII
	 * runes that case-fold to ASCII conservatively fall back to native. */
	if (ignore_case) {
		for (int path_index = 0; path_index < query->path_count; path_index++) {
			if (!path_exists[path_index]) {
				continue;
			}
			const char *root = query->paths[path_index];
			for (uint32_t i = 0; i < count; i++) {
				uint32_t index = le32(order_bytes + i * 4);
				helper_repo_rg_file file;
				if (index >= corpus->file_count || !helper_repo_rg_file_at(corpus, index, &file)) {
					return 0;
				}
				char path[PATH_BUF];
				memcpy(path, file.path, file.path_len);
				path[file.path_len] = '\0';
				if (file.fold_unsafe && helper_repo_rg_path_selects(root, path) &&
				    helper_repo_rg_globs_match(query, path)) {
					return 0;
				}
			}
		}
	}
	helper_repo_rg_seed seeds[HELPER_RG_MAX_SEED_MASKS];
	int seed_count = 0;
	(void)helper_repo_rg_seed_masks(query, ignore_case, seeds, &seed_count);
	regex_t regex;
	memset(&regex, 0, sizeof(regex));
	int use_regex = !query->fixed;
	char translated_regex[HELPER_RG_TRANSLATED_REGEX_BYTES];
	if (use_regex) {
		if (!helper_repo_rg_translate_regex(query->pattern, translated_regex, sizeof(translated_regex)) ||
		    regcomp(&regex, translated_regex, REG_EXTENDED | (ignore_case ? REG_ICASE : 0)) != 0) {
			return 0;
		}
	}
	byte_buf scratch = {0};
	int ok = 1;
	int matched_any = 0;
	for (int path_index = 0; ok && path_index < query->path_count && !(query->quiet && matched_any); path_index++) {
		if (!path_exists[path_index]) {
			continue;
		}
		const char *root = query->paths[path_index];
		for (uint32_t i = 0; ok && i < count && !(query->quiet && matched_any); i++) {
			uint32_t index = le32(order_bytes + i * 4);
			if (index >= corpus->file_count) {
				continue;
			}
			helper_repo_rg_file file;
			if (!helper_repo_rg_file_at(corpus, index, &file)) {
				ok = 0;
				break;
			}
			char path[PATH_BUF];
			memcpy(path, file.path, file.path_len);
			path[file.path_len] = '\0';
			if (!helper_repo_rg_path_selects(root, path) || !helper_repo_rg_globs_match(query, path)) {
				continue;
			}
			ok = helper_repo_rg_eval_file(query, &file, corpus, root, show_filename, &regex, use_regex,
			                              &scratch, &out->stdout_buf, &matched_any,
			                              seeds, seed_count, ignore_case);
		}
	}
	bytes_free(&scratch);
	if (use_regex) {
		regfree(&regex);
	}
	if (!ok) {
		return 0;
	}
	out->exit_code = missing_path ? 2 : (matched_any ? 0 : 1);
	return 1;
}

#define HELPER_GIT_HISTORY_HEADER_BYTES 32
#define HELPER_GIT_HISTORY_RECORD_BYTES 32
#define HELPER_GIT_HISTORY_PATH_BYTES 8
#define HELPER_GIT_HISTORY_MAX_COMMITS 512
#define HELPER_GIT_HISTORY_MAX_PATHS 32

typedef struct {
	int limit;
	int path_count;
	char paths[HELPER_GIT_HISTORY_MAX_PATHS][PATH_BUF];
} helper_git_history_query;

typedef struct {
	const unsigned char *data;
	uint32_t len;
	uint32_t commit_count;
	uint32_t records_offset;
	uint32_t path_records_offset;
	uint32_t path_count;
	uint32_t payload_offset;
	int complete;
} helper_git_history_corpus;

typedef struct {
	const unsigned char *hash;
	uint32_t hash_len;
	const unsigned char *oneline;
	uint32_t oneline_len;
	uint32_t path_start;
	uint32_t path_count;
	uint32_t parent_count;
} helper_git_history_commit;

static int helper_parse_positive_limit(const char *value, int *limit) {
	if (value == NULL || value[0] == '\0' || limit == NULL) {
		return 0;
	}
	char *end = NULL;
	errno = 0;
	long parsed = strtol(value, &end, 10);
	if (errno != 0 || end == value || *end != '\0' || parsed < 1 || parsed > 20) {
		return 0;
	}
	*limit = (int)parsed;
	return 1;
}

static int helper_git_history_parse(int argc, char **argv, helper_git_history_query *query) {
	if (query == NULL || argv == NULL || argc < 6 || strcmp(base_name(argv[0]), "git") != 0 ||
	    strcmp(argv[1], "log") != 0) {
		return 0;
	}
	memset(query, 0, sizeof(*query));
	int oneline = 0;
	int separator = 0;
	for (int i = 2; i < argc; i++) {
		const char *arg = argv[i];
		if (!separator && strcmp(arg, "--oneline") == 0) {
			oneline = 1;
			continue;
		}
		if (!separator && (strcmp(arg, "-n") == 0 || strcmp(arg, "--max-count") == 0)) {
			if (++i >= argc || query->limit != 0 || !helper_parse_positive_limit(argv[i], &query->limit)) {
				return 0;
			}
			continue;
		}
		if (!separator && strncmp(arg, "--max-count=", 12) == 0) {
			if (query->limit != 0 || !helper_parse_positive_limit(arg + 12, &query->limit)) {
				return 0;
			}
			continue;
		}
		if (!separator && arg[0] == '-' && arg[1] >= '0' && arg[1] <= '9') {
			if (query->limit != 0 || !helper_parse_positive_limit(arg + 1, &query->limit)) {
				return 0;
			}
			continue;
		}
		if (!separator && strcmp(arg, "--") == 0) {
			separator = 1;
			continue;
		}
		if (!separator || query->path_count >= HELPER_GIT_HISTORY_MAX_PATHS || arg[0] == ':' ||
		    strpbrk(arg, "*?[") != NULL) {
			return 0;
		}
		if (strcmp(arg, ".") == 0) {
			snprintf(query->paths[query->path_count], PATH_BUF, ".");
		} else if (!clean_relative_path(arg, query->paths[query->path_count])) {
			return 0;
		}
		query->path_count++;
	}
	return oneline && separator && query->limit > 0 && query->path_count > 0;
}

static int helper_git_history_decode_corpus(const unsigned char *data, uint32_t len,
	                                        helper_git_history_corpus *corpus) {
	static const unsigned char magic[8] = {'S', 'Q', 'G', 'I', 'T', 'H', '0', '1'};
	if (data == NULL || corpus == NULL || len < HELPER_GIT_HISTORY_HEADER_BYTES ||
	    len > MAX_GIT_HISTORY_CORPUS_BYTES || memcmp(data, magic, sizeof(magic)) != 0 || le32(data + 8) != 1) {
		return 0;
	}
	memset(corpus, 0, sizeof(*corpus));
	corpus->data = data;
	corpus->len = len;
	corpus->commit_count = le32(data + 12);
	corpus->records_offset = le32(data + 16);
	corpus->path_records_offset = le32(data + 20);
	corpus->payload_offset = le32(data + 24);
	corpus->complete = le32(data + 28) == 1;
	if (corpus->commit_count == 0 || corpus->commit_count > HELPER_GIT_HISTORY_MAX_COMMITS ||
	    corpus->records_offset != HELPER_GIT_HISTORY_HEADER_BYTES ||
	    corpus->path_records_offset < corpus->records_offset || corpus->payload_offset < corpus->path_records_offset ||
	    corpus->payload_offset > len ||
	    (corpus->path_records_offset - corpus->records_offset) != corpus->commit_count * HELPER_GIT_HISTORY_RECORD_BYTES ||
	    (corpus->payload_offset - corpus->path_records_offset) % HELPER_GIT_HISTORY_PATH_BYTES != 0 ||
	    (!corpus->complete && le32(data + 28) != 0)) {
		return 0;
	}
	corpus->path_count = (corpus->payload_offset - corpus->path_records_offset) / HELPER_GIT_HISTORY_PATH_BYTES;
	return 1;
}

static int helper_git_history_commit_at(const helper_git_history_corpus *corpus, uint32_t index,
	                                    helper_git_history_commit *commit) {
	if (corpus == NULL || commit == NULL || index >= corpus->commit_count) {
		return 0;
	}
	const unsigned char *record = corpus->data + corpus->records_offset + index * HELPER_GIT_HISTORY_RECORD_BYTES;
	uint32_t hash_offset = le32(record);
	uint32_t hash_len = le32(record + 4);
	uint32_t oneline_offset = le32(record + 8);
	uint32_t oneline_len = le32(record + 12);
	uint32_t path_start = le32(record + 16);
	uint32_t path_count = le32(record + 20);
	uint32_t parent_count = le32(record + 24);
	if (hash_len != 40 || oneline_len == 0 || hash_offset < corpus->payload_offset ||
	    oneline_offset < corpus->payload_offset || hash_offset > corpus->len || oneline_offset > corpus->len ||
	    hash_len > corpus->len - hash_offset || oneline_len > corpus->len - oneline_offset ||
	    path_start > corpus->path_count || path_count > corpus->path_count - path_start ||
	    memchr(corpus->data + hash_offset, '\0', hash_len) != NULL ||
	    memchr(corpus->data + oneline_offset, '\0', oneline_len) != NULL) {
		return 0;
	}
	commit->hash = corpus->data + hash_offset;
	commit->hash_len = hash_len;
	commit->oneline = corpus->data + oneline_offset;
	commit->oneline_len = oneline_len;
	commit->path_start = path_start;
	commit->path_count = path_count;
	commit->parent_count = parent_count;
	return 1;
}

static int helper_git_history_path_at(const helper_git_history_corpus *corpus, uint32_t index,
	                                  const unsigned char **path, uint32_t *path_len) {
	if (corpus == NULL || path == NULL || path_len == NULL || index >= corpus->path_count) {
		return 0;
	}
	const unsigned char *record = corpus->data + corpus->path_records_offset + index * HELPER_GIT_HISTORY_PATH_BYTES;
	uint32_t offset = le32(record);
	uint32_t len = le32(record + 4);
	if (len == 0 || len >= PATH_BUF || offset < corpus->payload_offset || offset > corpus->len ||
	    len > corpus->len - offset || memchr(corpus->data + offset, '\0', len) != NULL) {
		return 0;
	}
	*path = corpus->data + offset;
	*path_len = len;
	return 1;
}

static int helper_git_history_path_selected(const helper_git_history_query *query,
	                                        const unsigned char *path, uint32_t path_len) {
	for (int i = 0; i < query->path_count; i++) {
		const char *selected = query->paths[i];
		if (strcmp(selected, ".") == 0) {
			return 1;
		}
		size_t selected_len = strlen(selected);
		if ((selected_len == path_len && memcmp(selected, path, path_len) == 0) ||
		    (selected_len < path_len && memcmp(selected, path, selected_len) == 0 && path[selected_len] == '/')) {
			return 1;
		}
	}
	return 0;
}

static int helper_git_history_evaluate(const helper_git_history_query *query,
	                                   const helper_git_history_corpus *corpus, helper_result *out) {
	int selected_count = 0;
	for (uint32_t i = 0; i < corpus->commit_count && selected_count < query->limit; i++) {
		helper_git_history_commit commit;
		if (!helper_git_history_commit_at(corpus, i, &commit)) {
			return 0;
		}
		/* Path-limited Git traversal simplifies merges. Decline before the first
		 * merge rather than approximating that topology-dependent behavior. */
		if (commit.parent_count > 1) {
			return 0;
		}
		int selected = 0;
		for (uint32_t path_index = 0; path_index < commit.path_count; path_index++) {
			const unsigned char *path = NULL;
			uint32_t path_len = 0;
			if (!helper_git_history_path_at(corpus, commit.path_start + path_index, &path, &path_len)) {
				return 0;
			}
			if (helper_git_history_path_selected(query, path, path_len)) {
				selected = 1;
				break;
			}
		}
		if (selected) {
			if (!helper_copy_bytes(&out->stdout_buf, commit.oneline, commit.oneline_len) ||
			    !bytes_append_byte(&out->stdout_buf, '\n') || out->stdout_buf.len > MAX_FAST_OUTPUT_BYTES) {
				return 0;
			}
			selected_count++;
		}
	}
	if (selected_count < query->limit && !corpus->complete) {
		return 0;
	}
	out->exit_code = 0;
	return 1;
}

static int helper_prepare_git_history_at_cwd(const char *cwd, int argc, char **argv,
	                                         helper_result *out, long long replay_start_ns) {
	policy_invocation inv;
	if (!normalize_invocation_at_cwd(cwd, argc, argv, &inv)) {
		return 0;
	}
	helper_git_history_query query;
	if (!helper_git_history_parse(inv.argc, inv.argv, &query)) {
		return 0;
	}
	char repo_root[PATH_BUF], git_dir[PATH_BUF], store_root[PATH_BUF], epoch[256];
	if (!discover_git_dir(inv.cwd, repo_root, git_dir) || !discover_store_root(inv.cwd, store_root) ||
	    !git_history_corpus_epoch(&inv, epoch)) {
		return 0;
	}
	char root_key[HASH_HEX], command_input[128], command_hash[HASH_HEX];
	sha256_hex_str(repo_root, root_key);
	if (snprintf(command_input, sizeof(command_input), "git-history-corpus:%s", root_key) <= 0) {
		return 0;
	}
	sha256_hex_str(command_input, command_hash);
	mapped_snapshot snap = {0};
	const unsigned char *content = NULL, *stderr_data = NULL;
	uint32_t content_len = 0, stderr_len = 0;
	int exit_code = 0;
	uint64_t native_wall_ms = 0;
	if (!map_snapshot(store_root, &snap) ||
	    !snapshot_find(&snap, command_hash, epoch, HOT_KIND_GIT_HISTORY_CORPUS,
	                   &content, &content_len, &stderr_data, &stderr_len, &exit_code, &native_wall_ms) ||
	    stderr_len != 0 || exit_code != 0) {
		unmap_snapshot(&snap);
		return 0;
	}
	helper_git_history_corpus corpus;
	helper_result prepared = {0};
	int ok = helper_git_history_decode_corpus(content, content_len, &corpus) &&
	         helper_git_history_evaluate(&query, &corpus, &prepared);
	char verified_epoch[256];
	if (ok) {
		ok = git_history_corpus_epoch(&inv, verified_epoch) && strcmp(epoch, verified_epoch) == 0;
	}
	unmap_snapshot(&snap);
	if (!ok || !helper_note_replay(&prepared, store_root, (long long)native_wall_ms, replay_start_ns, 0)) {
		helper_result_free(&prepared);
		return 0;
	}
	*out = prepared;
	return 1;
}

static int helper_prepare_repo_search_at_cwd(const char *cwd, int argc, char **argv, helper_result *out,
	                                         long long replay_start_ns, int allow_missing_paths,
	                                         uint64_t shell_glob_mask) {
	policy_invocation raw_inv, inv;
	if (!normalize_invocation_at_cwd(cwd, argc, argv, &raw_inv)) {
		mmap_trace_path("repo-search-miss-policy", argc > 0 ? argv[0] : NULL);
		return 0;
	}
	if (shell_glob_mask != 0) {
		char *expanded_argv[MAX_ARGC];
		char expanded_storage[MAX_ARGC][PATH_BUF];
		int expanded_argc = 0;
		if (!helper_expand_shell_globs(raw_inv.cwd, raw_inv.argc, raw_inv.argv, shell_glob_mask,
		                               &expanded_argc, expanded_argv, expanded_storage) ||
		    !normalize_invocation_at_cwd(raw_inv.cwd, expanded_argc, expanded_argv, &inv)) {
			mmap_trace_path("repo-search-miss-glob", raw_inv.argv[0]);
			return 0;
		}
	} else {
		inv = raw_inv;
	}
	if (!is_fixed_rg_repo_search(&inv) && !is_bounded_rg_repo_search(&inv)) {
		mmap_trace_path("repo-search-miss-policy", inv.argv[0]);
		return 0;
	}
	helper_repo_rg_query query;
	if (!helper_repo_rg_parse(inv.argc, inv.argv, &query)) {
		mmap_trace_path("repo-search-miss-parse", inv.argc > 0 ? inv.argv[0] : NULL);
		return 0;
	}
	query.allow_missing_paths = allow_missing_paths;
	char repo_root[PATH_BUF], git_dir[PATH_BUF], store_root[PATH_BUF], epoch[256];
	if (!discover_git_dir(inv.cwd, repo_root, git_dir) || !discover_store_root(inv.cwd, store_root) ||
	    !repo_search_corpus_epoch(&inv, epoch)) {
		mmap_trace_path("repo-search-miss-proof", inv.cwd);
		return 0;
	}
	mmap_trace_path("repo-search-proof", epoch);
	char root_key[HASH_HEX], command_input[128], command_hash[HASH_HEX];
	sha256_hex_str(repo_root, root_key);
	if (snprintf(command_input, sizeof(command_input), "repo-search-corpus:%s", root_key) <= 0) {
		return 0;
	}
	sha256_hex_str(command_input, command_hash);
	mapped_snapshot snap = {0};
	const unsigned char *content = NULL, *stderr_data = NULL;
	uint32_t content_len = 0, stderr_len = 0;
	int exit_code = 0;
	uint64_t native_wall_ms = 0;
	if (!map_snapshot(store_root, &snap) ||
	    !snapshot_find(&snap, command_hash, epoch, HOT_KIND_REPO_SEARCH_CORPUS,
	                   &content, &content_len, &stderr_data, &stderr_len, &exit_code, &native_wall_ms) ||
	    stderr_len != 0 || exit_code != 0) {
		mmap_trace_path("repo-search-miss-snapshot", command_hash);
		unmap_snapshot(&snap);
		return 0;
	}
	helper_repo_rg_corpus corpus;
	helper_result prepared = {0};
	int ok = helper_repo_rg_decode_corpus(content, content_len, &corpus) &&
	         helper_repo_rg_evaluate(&query, &corpus, &prepared);
	char verified_epoch[256];
	if (ok) {
		ok = repo_search_corpus_epoch(&inv, verified_epoch) && strcmp(epoch, verified_epoch) == 0;
	}
	unmap_snapshot(&snap);
	if (!ok || !helper_note_replay(&prepared, store_root, (long long)native_wall_ms, replay_start_ns, 0)) {
		mmap_trace_path(ok ? "repo-search-miss-accounting" : "repo-search-miss-evaluate", inv.argv[0]);
		helper_result_free(&prepared);
		return 0;
	}
	mmap_trace_path("repo-search-hit", inv.argv[0]);
	*out = prepared;
	return 1;
}

static int helper_eval_source(helper_node *node, helper_result *out, long long replay_start_ns, const char *cwd) {
	char *argv[HELPER_SHELL_MAX_ARGS];
	for (int i = 0; i < node->argc; i++) {
		argv[i] = node->argv[i];
	}
	argv[node->argc] = NULL;
	mmap_trace_path("shell-ir-helper-source", argv[0]);
	if (node->argc == 1 && strcmp(base_name(argv[0]), "pwd") == 0) {
		char store_root[PATH_BUF];
		if (cwd == NULL || cwd[0] != '/' || !discover_store_root(cwd, store_root) ||
		    !helper_copy_bytes(&out->stdout_buf, (const unsigned char *)cwd, (uint32_t)strlen(cwd)) ||
		    !helper_copy_bytes(&out->stdout_buf, (const unsigned char *)"\n", 1)) {
			return 0;
		}
		out->exit_code = 0;
		return helper_note_replay(out, store_root, 0, replay_start_ns, 1);
	}
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
		if (ok && !helper_note_replay(out, prepared.store_root, (long long)prepared.native_wall_ms, replay_start_ns, 0)) {
			ok = 0;
		}
		release_prepared_exact_replay(&prepared);
		return ok;
	}
	mmap_trace_path("shell-ir-helper-source-exact-miss", argv[0]);
	if (helper_prepare_git_history_at_cwd(cwd, node->argc, argv, out, replay_start_ns)) {
		mmap_trace_path("shell-ir-helper-git-history-hit", argv[0]);
		return 1;
	}
	if (helper_prepare_repo_search_at_cwd(cwd, node->argc, argv, out, replay_start_ns,
	                                      node->stderr_discarded, node->shell_glob_mask)) {
		mmap_trace_path("shell-ir-helper-repo-search-hit", argv[0]);
		return 1;
	}
	return helper_prepare_warm_file_at_cwd(cwd, node->argc, argv, out, replay_start_ns);
}

static int helper_eval_nl_sed_pipe(helper_plan *plan, int left_idx, int right_idx, helper_result *out, long long replay_start_ns, const char *cwd) {
	if (plan == NULL || out == NULL || left_idx < 0 || left_idx >= plan->count || right_idx < 0 || right_idx >= plan->count) {
		return 0;
	}
	helper_node *left = &plan->nodes[left_idx];
	helper_node *right = &plan->nodes[right_idx];
	line_selection selection;
	if (left->kind != HELPER_NODE_EXEC || right->kind != HELPER_NODE_EXEC ||
	    left->argc != 3 || strcmp(base_name(left->argv[0]), "nl") != 0 || strcmp(left->argv[1], "-ba") != 0 ||
	    !helper_parse_stdin_sed(right, &selection) || !warm_file_replay_enabled()) {
		return 0;
	}
	char *argv[HELPER_SHELL_MAX_ARGS];
	for (int i = 0; i < left->argc; i++) {
		argv[i] = left->argv[i];
	}
	argv[left->argc] = NULL;
	policy_invocation inv;
	if (!normalize_invocation_at_cwd(cwd, left->argc, argv, &inv) || !is_warm_file_candidate(&inv)) {
		return 0;
	}
	char store_root[PATH_BUF];
	if (!discover_store_root(inv.cwd, store_root)) {
		return 0;
	}
	char key[HASH_HEX], epoch[256], path[PATH_BUF];
	warm_file_operation operation;
	unsigned char *current_content = NULL;
	size_t current_len = 0;
	if (!warm_file_proof(&inv, key, epoch, path, &operation, NULL, &current_content, &current_len)) {
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
	mapped_snapshot snap = {0};
	if (map_snapshot(store_root, &snap) &&
	    snapshot_find(&snap, command_hash, epoch, HOT_KIND_WARM_FILE, &content, &content_len, &err, &err_len, &exit_code, &native_wall_ms) &&
	    err_len == 0 && exit_code == 0) {
		int ok = helper_append_nl_selection(&out->stdout_buf, content, content_len, &selection, MAX_FILE_OUTPUT_BYTES);
		unmap_snapshot(&snap);
		free(current_content);
		if (ok) {
			out->exit_code = 0;
			ok = helper_note_replay(out, store_root, (long long)native_wall_ms, replay_start_ns, 0);
		}
		return ok;
	}
	unmap_snapshot(&snap);

	if (current_len > UINT32_MAX) {
		free(current_content);
		return 0;
	}
	int ok = helper_append_nl_selection(&out->stdout_buf, current_content, (uint32_t)current_len, &selection, MAX_FILE_OUTPUT_BYTES);
	free(current_content);
	if (ok) {
		out->exit_code = 0;
		ok = helper_note_replay(out, store_root, 0, replay_start_ns, 1);
	}
	return ok;
}

static int helper_eval_binary(helper_plan *plan, int left_idx, int right_idx, const byte_buf *input, helper_result *out, long long replay_start_ns, helper_node_kind kind, const char *cwd) {
	if (kind == HELPER_NODE_PIPE && input == NULL) {
		helper_result fused = {0};
		if (helper_eval_nl_sed_pipe(plan, left_idx, right_idx, &fused, replay_start_ns, cwd)) {
			*out = fused;
			return 1;
		}
		helper_result_free(&fused);
	}
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
		         helper_append_result(&out->stdout_buf, &right.stdout_buf) &&
		         helper_merge_replay(out, &left) && helper_merge_replay(out, &right);
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
	         helper_append_result(&out->stderr_buf, &right.stderr_buf) &&
	         helper_merge_replay(out, &left) && helper_merge_replay(out, &right);
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

static int helper_eval_shell_plan_at_cwd(const char *cwd, helper_plan *plan, helper_result *res, long long replay_start_ns) {
	if (res == NULL) {
		return 0;
	}
	memset(res, 0, sizeof(*res));
	if (plan == NULL || plan->root < 0 || plan->root >= plan->count) {
		return 0;
	}
	if (!helper_eval_node(plan, plan->root, NULL, res, replay_start_ns, cwd)) {
		mmap_trace_path("shell-ir-helper-eval-miss", NULL);
		helper_result_free(res);
		return 0;
	}
	mmap_trace_path("shell-ir-helper-eval-ok", NULL);
	if (res->stdout_buf.len > MAX_FAST_OUTPUT_BYTES || res->stderr_buf.len > MAX_FAST_OUTPUT_BYTES) {
		helper_result_free(res);
		return 0;
	}
	return 1;
}

static int helper_eval_shell_ir_at_cwd(const char *cwd, const char *command, helper_result *res, long long replay_start_ns) {
	mmap_trace_path("shell-ir-helper-run-enter", NULL);
	if (res == NULL) {
		return 0;
	}
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
	int ok = helper_eval_shell_plan_at_cwd(cwd, plan, res, replay_start_ns);
	free(plan);
	return ok;
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
