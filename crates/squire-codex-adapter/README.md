# Squire Codex Adapter

This directory owns the source that adapts Squire replay results into Codex
execution output types. It is included by `squire-codex` using Rust `#[path]`
modules so the fork keeps only tiny hook sites while the adapter source remains
in the Squire repository.
