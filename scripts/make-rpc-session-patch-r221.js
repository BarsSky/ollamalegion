// R221/R226: пересобрать docker/cppworker/patches/0003-rpc-session-loss.patch из чистого
// ggml-rpc.cpp (правки заданы явно, затем diff). Запускать из корня репозитория:
//   node scripts/make-rpc-session-patch-r221.js
// После генерации ОБЯЗАТЕЛЬНО проверить, что сборка образа применяет патч: имя патча
// и его переводы строк должны совпадать с исходником (см. тесты R221/R226).
const fs = require('fs');
const path = require('path');
const cp = require('child_process');

const SRC = 'c/llama.cpp/ggml/src/ggml-rpc/ggml-rpc.cpp';
const REL = 'ggml/src/ggml-rpc/ggml-rpc.cpp';
const ROOT = '_diag/r221';
const A = path.join(ROOT, 'a', REL);
const B = path.join(ROOT, 'b', REL);

fs.rmSync(ROOT, { recursive: true, force: true });
for (const p of [A, B]) fs.mkdirSync(path.dirname(p), { recursive: true });

const pristine = fs.readFileSync(SRC, 'utf8').replace(/\r\n/g, '\n');
fs.writeFileSync(A, pristine, 'utf8');

let text = pristine;
function sub(from, to) {
  const n = text.split(from).length - 1;
  if (n !== 1) throw new Error('expected 1 match, got ' + n + ' for: ' + from.slice(0, 60));
  text = text.replace(from, to);
}

// 1. the abort macro goes away: a lost session is reported, not fatal
sub(
`// macro for nicer error messages on server crash
#define RPC_STATUS_ASSERT(x) if (!(x)) GGML_ABORT("Remote RPC server crashed or returned malformed response")
`,
``);

// 2. handshake failure returns false so start() can throw (patch 0002 path)
sub(
`    bool status = send_rpc_cmd(sock, RPC_CMD_HELLO, &request, sizeof(request), &response, sizeof(response));
    RPC_STATUS_ASSERT(status);
`,
`    bool status = send_rpc_cmd(sock, RPC_CMD_HELLO, &request, sizeof(request), &response, sizeof(response));
    if (!status) {
        GGML_LOG_ERROR("%s: RPC server did not answer the hello handshake\\n", __func__);
        return false;
    }
`);

// 3. dispatcher: expose session state
sub(
`    void synchronize();

    void start(const std::string & endpoint);`,
`    void synchronize();

    bool failed() const;

    void start(const std::string & endpoint);`);

sub(
`    rpc_msg_queue    queue;
    socket_ptr       sock;
    std::atomic_bool running;
    std::thread      thread;`,
`    void fail(const std::string & what);

    rpc_msg_queue    queue;
    socket_ptr       sock;
    std::atomic_bool running;
    std::atomic_bool failed_flag = false;
    std::thread      thread;`);

// 4. worker thread: fail fast instead of abort, keep draining the queue so no
//    caller waits forever on a promise that would never be set
sub(
`        if (msg_ptr->cmd != RPC_CMD_NONE) {
            if (msg_ptr->output) {
                bool status = send_rpc_cmd(sock, msg_ptr->cmd, msg_ptr->input.get(), msg_ptr->input_size, msg_ptr->output, msg_ptr->output_size);
                RPC_STATUS_ASSERT(status);
            } else {
                bool status = send_rpc_cmd(sock, msg_ptr->cmd, msg_ptr->input.get(), msg_ptr->input_size);
                RPC_STATUS_ASSERT(status);
            }
        }
        msg_ptr->completion.set_value();`,
`        if (msg_ptr->cmd != RPC_CMD_NONE) {
            bool ok = !failed_flag.load();
            if (ok) {
                if (msg_ptr->output) {
                    ok = send_rpc_cmd(sock, msg_ptr->cmd, msg_ptr->input.get(), msg_ptr->input_size, msg_ptr->output, msg_ptr->output_size);
                } else {
                    ok = send_rpc_cmd(sock, msg_ptr->cmd, msg_ptr->input.get(), msg_ptr->input_size);
                }
                if (!ok) {
                    fail("command " + std::to_string((int) msg_ptr->cmd) + " failed");
                }
            }
            if (!ok && msg_ptr->output) {
                memset(msg_ptr->output, 0, msg_ptr->output_size);
            }
        }
        msg_ptr->completion.set_value();`);

sub(
`rpc_dispatcher::~rpc_dispatcher() {`,
`bool rpc_dispatcher::failed() const {
    return failed_flag.load();
}

void rpc_dispatcher::fail(const std::string & what) {
    if (!failed_flag.exchange(true)) {
        GGML_LOG_ERROR("%s: RPC session lost, remote endpoint is gone: %s\\n", __func__, what.c_str());
    }
}

rpc_dispatcher::~rpc_dispatcher() {`);

// 5. compute surfaces the lost session as GGML_STATUS_FAILED
sub(
`    GGML_ASSERT(cgraph->n_nodes > 0);
    bool reuse = cgraph->uid != 0 && rpc_dev_ctx->last_graph_uid == cgraph->uid;`,
`    GGML_ASSERT(cgraph->n_nodes > 0);
    if (rpc_ctx->dispatcher->failed()) {
        return GGML_STATUS_FAILED;
    }

    bool reuse = cgraph->uid != 0 && rpc_dev_ctx->last_graph_uid == cgraph->uid;`);

fs.writeFileSync(B, text, 'utf8');

// byte-exact diff through git, then normalise the header paths to a/ and b/
let raw;
try {
  raw = cp.execFileSync('git', ['diff', '--no-index', '--no-color', `a/${REL}`, `b/${REL}`], {
    cwd: ROOT,
    maxBuffer: 1 << 26,
  });
} catch (e) {
  if (e.status !== 1) throw e;
  raw = e.stdout;
}
let diff = raw.toString('utf8');
const lines = diff.split('\n');
for (let i = 0; i < Math.min(4, lines.length); i++) {
  lines[i] = lines[i]
    .replace(/^diff --git a\/a\//, 'diff --git a/')
    .replace(/ b\/b\//, ' b/')
    .replace(/^--- a\/a\//, '--- a/')
    .replace(/^\+\+\+ b\/b\//, '+++ b/');
}
diff = lines.join('\n');

// ВАЖНО: исходники сабмодуля в рабочем дереве и в build-контексте — CRLF (как и
// патчи 0001/0002). Патч с одиночными LF `git apply` в образе не примет
// («patch does not apply»), при этом локальная проверка на копии проходит.
diff = diff.replace(/\r\n/g, '\n').replace(/\n/g, '\r\n');
if (/(?<!\r)\n/.test(diff)) throw new Error('в патче остались одиночные LF');
// guard: header must name the repo-relative path, not the temp tree
if (!/^--- a\/ggml\/src\/ggml-rpc\/ggml-rpc\.cpp/m.test(diff)) throw new Error('bad header\n' + diff.slice(0, 400));
if (/\/a\/ggml|\/b\/ggml/.test(diff.split('\n').slice(0, 5).join('\n'))) throw new Error('temp path leaked');

const out = 'docker/cppworker/patches/0003-rpc-session-loss.patch';
fs.writeFileSync(out, diff, 'utf8');
console.log('wrote ' + out + ' (' + diff.length + ' bytes)');
console.log(diff.split('\n').slice(0, 4).join('\n'));
