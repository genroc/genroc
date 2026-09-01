#!/usr/bin/env node
// npm `bin` entries need a shebang and a stable extension; Node's type stripping handles the
// .ts behind it. genroc.yaml points here: command: [npx, genroc-import]
import "../import.ts";
