#!/usr/bin/env groovy

// daggerKubernetes — Jenkins shared library for the dagger-kubernetes platform.
//
// Two modes:
//   * default: runs the Dagger command (via `body`) with the platform env vars
//     set, then prints the pipeline-view link.
//   * dynamicStages: launches the `dagger-kubernetes-ci` wrapper with `--steps`
//     in the background and renders Dagger's internal step tree as nested
//     scripted-pipeline `stage()` blocks (Blue Ocean) with per-stage logs and
//     statuses. See docs/design/ADR-024-ci-nested-steps.md.
def call(Map params = [:], Closure body = null) {
    String serverUrl = params.serverUrl ?: env.DAGGER_KUBERNETES_SERVER
    String token = params.token ?: env.DAGGER_KUBERNETES_TOKEN
    String uiUrl = params.uiUrl ?: env.DAGGER_KUBERNETES_UI ?: serverUrl
    String version = params.version ?: env.DAGGER_TAG

    // Auto-discover the installed Dagger CLI version when no explicit tag is set.
    if (!version) {
        try {
            String versionOut = sh(script: "dagger version 2>/dev/null || true", returnStdout: true).trim()
            if (versionOut) {
                def matcher = versionOut =~ /v(\d+\.\d+\.\d+)/
                if (matcher) {
                    version = "v${matcher[0][1]}"
                    echo "Auto-discovered Dagger CLI version: ${version}"
                }
            }
        } catch (Exception ignored) {
            // dagger not available — skip auto-discovery
        }
    }

    boolean dynamicStages = envTruthy(params.dynamicStages, env.DAGGER_KUBERNETES_DYNAMIC_STAGES, false)
    String stepsPollInterval = params.stepsPollInterval ?: env.DAGGER_KUBERNETES_STEPS_POLL_INTERVAL ?: '2s'
    int stepsMaxDepth = (params.stepsMaxDepth ?: env.DAGGER_KUBERNETES_STEPS_MAX_DEPTH ?: 8) as int
    int timeoutMinutes = (params.timeoutMinutes ?: env.DAGGER_KUBERNETES_TIMEOUT_MINUTES ?: 30) as int
    boolean magicCache = envTruthy(params.magicCache, env.DAGGER_KUBERNETES_MAGIC_CACHE, false)
    String cacheRegistry = params.cacheRegistry ?: env.DAGGER_KUBERNETES_CACHE_REGISTRY ?: 'cache.reg/dagger-cache'

    if (!serverUrl || !token) {
        error "daggerKubernetes: serverUrl and token are required"
    }

    if (params.provisionCli) {
        provisionCli(serverUrl: serverUrl, token: token,
                     version: params.cliVersion ?: env.DAGGER_KUBERNETES_CLI_VERSION,
                     os: params.cliOs, arch: params.cliArch)
    }

    String cacheConfig = ''
    if (magicCache) {
        assertShellSafe(cacheRegistry, 'cacheRegistry')
        cacheConfig = "type=registry,ref=${cacheRegistry}:cache,mode=max"
    }

    if (dynamicStages) {
        dynamicStagesRun(serverUrl: serverUrl, token: token, uiUrl: uiUrl,
                         version: version, stepsPollInterval: stepsPollInterval,
                         stepsMaxDepth: stepsMaxDepth, timeoutMinutes: timeoutMinutes,
                         command: params.command, cacheConfig: cacheConfig)
        return
    }

    withEnv([
        "DAGGER_CLOUD_URL=${serverUrl}",
        "DAGGER_CLOUD_TOKEN=${token}",
        "_EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self"
    ]) {
        if (version) {
            env._EXPERIMENTAL_DAGGER_TAG = version
        }
        if (cacheConfig) {
            env._EXPERIMENTAL_DAGGER_CACHE_CONFIG = cacheConfig
        }

        if (body) {
            try {
                body()
            } catch (e) {
                echo "[dagger-kubernetes] Pipeline failed. View: ${uiUrl}/traces/latest"
                throw e
            }
        } else if (!dynamicStages) {
            error "daggerKubernetes: provide a closure body or set dynamicStages: true with command: '...'"
        }
    }
}

// envTruthy resolves a flag that may come from a map param or an env var. Env
// vars are always strings, so "false"/"0"/"no"/"off" must count as false —
// Groovy's default truthiness would treat any non-empty string as true.
boolean envTruthy(def mapValue, def envValue, boolean deflt) {
    if (mapValue != null) {
        if (mapValue instanceof String) {
            return parseBool(mapValue, deflt)
        }
        return mapValue as boolean
    }
    if (envValue == null || envValue == '') {
        return deflt
    }
    return parseBool(envValue.toString(), deflt)
}

// parseBool interprets common string representations of booleans.
boolean parseBool(String s, boolean deflt) {
    String v = s.trim().toLowerCase()
    if (v in ['false', '0', 'no', 'off']) {
        return false
    }
    if (v in ['true', '1', 'yes', 'on']) {
        return true
    }
    return deflt
}

// dynamicStagesRun launches the dagger-kubernetes-ci wrapper with --steps in the
// background of the enclosing node, streams its NDJSON event output, and renders
// nested stage() blocks from the reconstructed step tree. The Dagger command is
// passed via the `command` param (a single shell string the wrapper executes).
void dynamicStagesRun(Map params = [:]) {
    String serverUrl = params.serverUrl
    String token = params.token
    String uiUrl = params.uiUrl
    String version = params.version
    String stepsPollInterval = params.stepsPollInterval
    int stepsMaxDepth = params.stepsMaxDepth
    int timeoutMinutes = params.timeoutMinutes
    String daggerCommand = params.command ?: env.DAGGER_COMMAND
    String cacheConfig = params.cacheConfig ?: ''

    if (!daggerCommand) {
        error "daggerKubernetes(dynamicStages: true): pass `command: 'dagger call ...'` (or set env.DAGGER_COMMAND)"
    }

    // Every value interpolated into the launch script below must be validated:
    // a value carrying a quote would break out of the single-quoted shell
    // contexts, and an unquoted metacharacter would inject shell syntax
    // (CWE-78). daggerCommand is the deliberate exception — it is a shell
    // command string authored by the trusted pipeline author.
    assertShellSafe(serverUrl, 'serverUrl')
    assertShellSafe(uiUrl, 'uiUrl')
    assertShellSafe(stepsPollInterval, 'stepsPollInterval')
    assertShellSafe(version, 'version')
    String wrapper = env.DAGGER_KUBERNETES_CI_BIN ?: 'dagger-kubernetes-ci'
    if (!(wrapper ==~ /[A-Za-z0-9._\/-]+/)) {
        error "daggerKubernetes: DAGGER_KUBERNETES_CI_BIN must be a plain binary name or path"
    }

    String stepsDir = "${WORKSPACE}/.dagger-kubernetes"
    String ndjsonFile = "${stepsDir}/steps-${env.BUILD_NUMBER}.ndjson"
    String stderrFile = "${stepsDir}/dagger-${env.BUILD_NUMBER}.log"
    String exitFile = "${stepsDir}/exit-${env.BUILD_NUMBER}"
    String pidFile = "${stepsDir}/pid-${env.BUILD_NUMBER}"
    [ndjsonFile, stderrFile, exitFile, pidFile].each { assertShellSafe(it, 'workspace file path') }

    // The token is exported into the environment and consumed by the wrapper
    // through its DAGGER_KUBERNETES_TOKEN env source. The script below only
    // references the variable NAME (Groovy interpolates the escaped '$'
    // literally), so the build log never records the value — and because the
    // wrapper reads it from the environment instead of a --token argument, the
    // value never appears in the wrapper's process argv either (argv is
    // readable by every local user via ps / /proc/<pid>/cmdline, CWE-214).
    withEnv(["DAGGER_KUBERNETES_TOKEN=${token}"] + (cacheConfig ? ["_EXPERIMENTAL_DAGGER_CACHE_CONFIG=${cacheConfig}"] : [])) {
        sh "mkdir -p '${stepsDir}'"
        String versionArgs = version ? "--version '${version}'" : ''

        // Launch the wrapper in the background. The subshell's own stdout/stderr
        // are redirected to /dev/null so the `sh` step returns immediately
        // instead of waiting for the background process to close the log pipe
        // (which would otherwise hang the enclosing node until the run ends).
        sh """
            set +e
            ( ${wrapper} --server '${serverUrl}' \\
                --ui-url '${uiUrl}' --steps \\
                --steps-poll-interval '${stepsPollInterval}' \\
                --steps-max-depth '${stepsMaxDepth}' ${versionArgs} \\
                ${daggerCommand} > '${ndjsonFile}' 2> '${stderrFile}'
              echo \$? > '${exitFile}' ) > /dev/null 2>&1 &
            echo \$! > '${pidFile}'
        """
    }

    timeout(time: timeoutMinutes, unit: 'MINUTES') {
        renderStepTree(ndjsonFile: ndjsonFile, stderrFile: stderrFile,
                       exitFile: exitFile, pidFile: pidFile, uiUrl: uiUrl)
    }
}

// renderStepTree consumes the wrapper's NDJSON event stream from ndjsonFile and
// renders nested scripted-pipeline stage() blocks. It reads the file
// incrementally (only new lines since the last poll) so it never re-echoes old
// lines, accumulates the reconstructed node tree, and renders it recursively
// once the wrapper signals pipeline_done. On completion it waits for the
// wrapper, prints its stderr + the pipeline-view link, and propagates the
// wrapper's exit status as the build result.
void renderStepTree(Map params = [:]) {
    String ndjsonFile = params.ndjsonFile
    String stderrFile = params.stderrFile
    String exitFile = params.exitFile
    String pidFile = params.pidFile
    String uiUrl = params.uiUrl

    int offset = 0
    boolean done = false
    String finalStatus = 'success'

    // node id -> [id, name, parent_id, depth, state, error, logs[], children[]]
    // (plain maps/lists so they stay CPS-serializable across poll iterations).
    def nodes = [:]
    def rootId = null
    boolean wrapperExited = false

    while (!done) {
        // Liveness: the exit file is only written after the wrapper process
        // has terminated, so once it exists the NDJSON stream is final. If
        // pipeline_done is still missing at that point, the wrapper died
        // before emitting its guaranteed terminal event (crash, OOM kill,
        // hang-then-kill) and polling further would only spin "Sleeping for
        // 1 sec" until the enclosing timeout — fail fast instead.
        boolean exited = fileExists(exitFile)

        String raw = ''
        try {
            raw = readFile(file: ndjsonFile)
        } catch (Exception ignored) {
            // The wrapper may not have created the file yet on the first
            // iterations; an empty stream is just "no events yet".
        }
        if (raw.length() > offset) {
            String newText = raw.substring(offset)
            int lastNL = newText.lastIndexOf('\n')
            if (lastNL >= 0) {
                String complete = newText.substring(0, lastNL + 1)
                offset += complete.length()
                for (String line : complete.split('\n')) {
                    line = line.trim()
                    if (!line) { continue }
                    // Parse AND dispatch inside the guard: a line that is
                    // malformed JSON or a well-formed object of the wrong
                    // shape (missing node/log fields) is skipped, never fatal.
                    try {
                        def evt = readJSON(text: line)
                        switch (evt.type) {
                            case 'node_started':
                                def id = evt.node.id
                                if (!nodes.containsKey(id)) {
                                    nodes[id] = [
                                        id: id,
                                        name: normalizeStageName(evt.node.name, id),
                                        parent_id: evt.node.parent_id ?: '',
                                        depth: evt.node.depth ?: 0,
                                        state: evt.node.state ?: 'running',
                                        error: '',
                                        logs: [],
                                        children: [],
                                    ]
                                    if (rootId == null || (evt.node.parent_id ?: '') == '') {
                                        rootId = id
                                    }
                                }
                                break
                            case 'node_finished':
                                def n = nodes[evt.node.id]
                                if (n != null) {
                                    n.state = evt.node.state ?: 'succeeded'
                                    n.error = evt.error ?: ''
                                }
                                break
                            case 'log_chunk':
                                def owner = nodes[evt.log.node_id]
                                if (owner != null) {
                                    for (String l : evt.log.lines) {
                                        owner.logs.add(l)
                                    }
                                }
                                break
                            case 'pipeline_done':
                                finalStatus = evt.status ?: 'success'
                                done = true
                                break
                        }
                    } catch (Exception e) {
                        echo "[dagger-kubernetes] skipping malformed step event: ${line}"
                        continue
                    }
                }
            }
        }
        if (!done) {
            if (exited) {
                wrapperExited = true
                finalStatus = 'failed'
                break
            }
            sleep(time: 1, unit: 'SECONDS')
        }
    }

    if (wrapperExited) {
        echo "[dagger-kubernetes] wrapper exited without a terminal pipeline_done event; treating the run as failed"
    }

    // Link children to parents (child-before-parent finish is fine: both are
    // already fully present once pipeline_done has arrived).
    for (def e : nodes.entrySet()) {
        def node = e.value
        String parentId = node.parent_id
        if (parentId && nodes.containsKey(parentId)) {
            nodes[parentId].children.add(node.id)
        }
    }

    // Render the full nested tree. The root wraps everything; child subtrees
    // render in order with their own logs and statuses. The visited set guards
    // against a forged cyclic parent chain (defense-in-depth: the wrapper
    // emits a tree, but the file is on a shared workspace).
    if (rootId != null && nodes.containsKey(rootId)) {
        renderNode(nodes, rootId, [] as Set)
    } else {
        stage("dagger") {
            echo "[dagger-kubernetes] no step tree was captured (status ${finalStatus})"
        }
    }

    // Wait for the wrapper to exit and read its exit code (authoritative).
    // The pid is validated as digits before it reaches the shell: the file
    // lives in the workspace, and anything a previous build step wrote there
    // is untrusted input (CWE-78).
    String pid = ''
    try {
        pid = readFile(file: pidFile).trim()
    } catch (Exception ignored) {
    }
    if (pid && !(pid ==~ /\d+/)) {
        echo "[dagger-kubernetes] ignoring malformed wrapper pid file"
        pid = ''
    }
    if (pid) {
        // Poll for the process to disappear, bounded by the enclosing timeout.
        String alive = sh(script: "kill -0 ${pid} 2>/dev/null && echo yes || echo no", returnStdout: true).trim()
        while (alive == 'yes') {
            sleep(time: 1, unit: 'SECONDS')
            alive = sh(script: "kill -0 ${pid} 2>/dev/null && echo yes || echo no", returnStdout: true).trim()
        }
    }
    String exitCode = '1'
    try {
        exitCode = readFile(file: exitFile).trim()
    } catch (Exception ignored) {
    }
    String stderr = ''
    try {
        stderr = readFile(file: stderrFile)
    } catch (Exception ignored) {
    }
    if (stderr.trim()) {
        echo stderr.trim()
    }
    echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/pipelines/${finalTraceId(stderr)}"

    // Best-effort cleanup: the per-build stream files must not accumulate in
    // the workspace across builds (disk exhaustion, CWE-400). The paths were
    // validated as shell-safe before the launch script was built.
    sh "rm -f '${ndjsonFile}' '${stderrFile}' '${exitFile}' '${pidFile}'"

    boolean failed = exitCode != '0' || finalStatus == 'failed' || finalStatus == 'canceled'
    if (failed) {
        currentBuild.result = 'FAILURE'
        error "dagger-kubernetes: pipeline failed (exit ${exitCode})"
    }
}

// finalTraceId extracts the trace id from the wrapper's stderr for the
// pipeline-view link; empty when none was captured.
String finalTraceId(String stderr) {
    def m = (stderr ?: '') =~ /[a-f0-9]{32,}/
    return m ? m[0] : ''
}

// renderNode recursively renders one step node as a nested stage: opens the
// stage, renders its children in order, echoes its own log lines, and records
// per-stage failure via catchError so the build can still render siblings
// before the final result is applied. The visited set makes the recursion
// terminate even on a forged cyclic parent chain (defense-in-depth).
void renderNode(def nodes, String nodeId, Set visited) {
    if (!visited.add(nodeId)) {
        return
    }
    def node = nodes[nodeId]
    String name = node.name
    boolean isFailed = node.state == 'failed'

    if (isFailed) {
        catchError(stageResult: 'FAILURE') {
            stage(name) {
                for (String childId : node.children) {
                    renderNode(nodes, childId, visited)
                }
                for (String l : node.logs) {
                    echo l
                }
                echo "[dagger-kubernetes] stage '${name}' failed: ${node.error ?: 'unknown error'}"
            }
        }
    } else {
        stage(name) {
            for (String childId : node.children) {
                renderNode(nodes, childId, visited)
            }
            for (String l : node.logs) {
                echo l
            }
        }
    }
}

// isShellUnsafe reports whether value contains a character that could break
// out of a quoted shell context or trigger expansion when the value is
// interpolated into an sh script: quotes, backslash, dollar, backtick, or any
// control character (CWE-78). Values failing this check must never be
// interpolated into sh steps.
boolean isShellUnsafe(String value) {
    for (char c : (value ?: '').toCharArray()) {
        if (c == '\'' || c == '"' || c == '\\' || c == '$' || c == '`' || Character.isISOControl(c)) {
            return true
        }
    }
    return false
}

// assertShellSafe fails the build when value is not safe to interpolate into
// an sh script (see isShellUnsafe).
void assertShellSafe(String value, String what) {
    if (value != null && isShellUnsafe(value)) {
        error "daggerKubernetes: ${what} must not contain quotes, backslashes, dollar signs, backticks, or control characters"
    }
}

// normalizeStageName sanitizes a span name for use as a Jenkins stage name:
// control characters are stripped, whitespace runs collapse to a single space,
// the result is capped in length, and an empty result falls back to a short
// id-derived name.
String normalizeStageName(String name, String id) {
    String clean = (name ?: '').replaceAll(/[\p{Cntrl}]/, '').replaceAll(/\s+/, ' ').trim()
    if (clean.length() > 80) {
        clean = clean.substring(0, 80)
    }
    if (!clean) {
        clean = "step-${(id ?: '').take(8)}"
    }
    return clean
}

def withStages(serverUrl, token, uiUrl) {
    echo "[dagger-kubernetes] Jenkins shared library loaded"
    echo "  Server: ${serverUrl}"
    echo "  UI: ${uiUrl}"
}

def provisionCli(Map params = [:]) {
    String serverUrl = params.serverUrl ?: env.DAGGER_KUBERNETES_SERVER
    String token = params.token ?: env.DAGGER_KUBERNETES_TOKEN
    String version = params.version ?: env.DAGGER_KUBERNETES_CLI_VERSION ?: ''
    String osName = params.os ?: 'linux'
    String arch = params.arch ?: 'amd64'

    if (!serverUrl || !token) {
        error "provisionCli: serverUrl and token are required"
    }

    // Everything interpolated into the curl commands below is validated: a
    // quote, dollar, or backtick would break out of the quoted shell context
    // (CWE-78), and the download URL must additionally be an http(s) URL so a
    // compromised supervisor response cannot turn it into curl options.
    assertShellSafe(serverUrl, 'serverUrl')
    assertShellSafe(version, 'version')
    assertShellSafe(osName, 'os')
    assertShellSafe(arch, 'arch')

    String binDir = "${WORKSPACE}/.dagger-cli"
    assertShellSafe(binDir, 'workspace path')

    // The Authorization header is written to a workspace file and passed to
    // curl via -H @file (curl >= 7.55): the token never appears in the build
    // log, in curl's process argv (readable by every local user via ps), nor
    // in the build-wide environment (CWE-214/CWE-532). The file is deleted as
    // soon as provisioning finishes.
    def headerFile = "${WORKSPACE}/.dagger-kubernetes-auth-${env.BUILD_NUMBER}.hdr"
    assertShellSafe(headerFile, 'header file path')
    writeFile(file: headerFile, text: "Authorization: Bearer ${token}")
    sh "chmod 600 '${headerFile}'"
    try {
        String downloadUrl
        if (version) {
            downloadUrl = "${serverUrl}/api/v1/cli/${version}?os=${osName}&arch=${arch}"
        } else {
            String latestUrl = "${serverUrl}/api/v1/cli/versions/latest?os=${osName}&arch=${arch}"
            String latest = sh(script: "curl -fsS -H @'${headerFile}' '${latestUrl}'", returnStdout: true).trim()
            def json = readJSON(text: latest)
            downloadUrl = json.url
        }
        if (downloadUrl == null || !(downloadUrl ==~ /https?:\/\/\S+/) || isShellUnsafe(downloadUrl)) {
            error "provisionCli: supervisor returned an unsafe CLI download URL"
        }

        sh """
            mkdir -p "${binDir}"
            curl -fsS -H @'${headerFile}' '${downloadUrl}' | tar xz -C "${binDir}"
            chmod +x "${binDir}/dagger"
        """
    } finally {
        sh "rm -f '${headerFile}'"
    }
    env.PATH = "${binDir}:${env.PATH}"
    echo "[dagger-kubernetes] Provisioned Dagger CLI (${version ?: 'latest'}) at ${binDir}"
}
