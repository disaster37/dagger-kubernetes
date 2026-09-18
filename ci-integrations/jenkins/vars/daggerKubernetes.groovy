#!/usr/bin/env groovy

// daggerKubernetes — Jenkins shared library for the dagger-kubernetes platform.
//
// Two modes:
//   * default: runs the Dagger command (via `body`) with the platform env vars
//     set, then prints the pipeline-view link.
//   * dynamicStages: runs the Dagger command directly in two clean stages:
//     "Provision Dagger CLI" (conditional) and "Dagger" (plain-text output).
def call(Map params = [:], Closure body = null) {
    String serverUrl = params.serverUrl ?: env.DAGGER_KUBERNETES_SERVER
    String token = (params.token ?: env.DAGGER_KUBERNETES_TOKEN)?.trim()
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
    int timeoutMinutes = resolveTimeoutMinutes(params.timeoutMinutes ?: env.DAGGER_KUBERNETES_TIMEOUT_MINUTES)

    if (!serverUrl || !token) {
        error "daggerKubernetes: serverUrl and token are required"
    }

	if (dynamicStages) {
		String daggerCommand = params.command ?: env.DAGGER_COMMAND
		if (!daggerCommand) {
			error "daggerKubernetes(dynamicStages: true): pass `command: 'dagger call ...'` (or set env.DAGGER_COMMAND)"
		}

		// Stage 1: Provision Dagger CLI (only when provisionCli is enabled).
		if (params.provisionCli) {
			stage("Provision Dagger CLI") {
				provisionCli(serverUrl: serverUrl, token: token,
							 version: params.cliVersion ?: env.DAGGER_KUBERNETES_CLI_VERSION,
							 os: params.cliOs, arch: params.cliArch)
			}
		}

		// Stage 2: Run the dagger command with real-time plaintext streaming.
		stage("Dagger") {
			// Private temp dir (0700) so a local user on a shared agent cannot
			// pre-create or replace the trace-id file with a symlink
			// (CWE-59/CWE-377). The wrapper writes the file inside it; the
			// finally block removes the whole dir.
			String traceDir = sh(script: 'mktemp -d /tmp/dagger-trace-XXXXXX', returnStdout: true).trim()
			assertShellSafe(traceDir, 'trace-id dir path')
			String traceIdFile = "${traceDir}/trace.id"
			assertShellSafe(traceIdFile, 'trace-id file path')
			withEnv([
				"DAGGER_CLOUD_URL=${serverUrl}",
				"DAGGER_CLOUD_TOKEN=${token}",
				"DAGGER_KUBERNETES_TOKEN=${token}",
				"DAGGER_KUBERNETES_TRACE_ID_FILE=${traceIdFile}",
				"_EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self"
			] + (version ? ["_EXPERIMENTAL_DAGGER_TAG=${version}"] : [])) {
				if (timeoutMinutes > 0) {
					timeout(time: timeoutMinutes, unit: 'MINUTES') {
						runDaggerCommand(daggerCommand, serverUrl, uiUrl, traceIdFile, traceDir)
					}
				} else {
					runDaggerCommand(daggerCommand, serverUrl, uiUrl, traceIdFile, traceDir)
				}
			}
		}
		return
	}

    if (params.provisionCli) {
        provisionCli(serverUrl: serverUrl, token: token,
                     version: params.cliVersion ?: env.DAGGER_KUBERNETES_CLI_VERSION,
                     os: params.cliOs, arch: params.cliArch)
    }

    withEnv([
        "DAGGER_CLOUD_URL=${serverUrl}",
        "DAGGER_CLOUD_TOKEN=${token}",
        "_EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self"
    ]) {
        if (version) {
            env._EXPERIMENTAL_DAGGER_TAG = version
        }

        if (body) {
            try {
                body()
            } catch (e) {
                echo "[dagger-kubernetes] Pipeline failed. View: ${uiUrl}/traces/latest"
                throw e
            }
        } else {
            error "daggerKubernetes: provide a closure body or set dynamicStages: true with command: '...'"
        }
    }
}

// runDaggerCommand runs the dagger command with stderr/stdout streaming live to
// the console. The wrapper (when present) prints
// "[dagger-kubernetes-ci] Pipeline View (live): <url>" as soon as it discovers
// the trace ID, and writes the ID to traceIdFile (via
// DAGGER_KUBERNETES_TRACE_ID_FILE); the finally block reads it to print the
// end-of-stage link, falling back to /traces/latest. --ui-url is passed so the
// wrapper's links use the correct base (otherwise the wrapper falls back to the
// compiled-in server.public_url because --config /dev/null "exists").
void runDaggerCommand(String daggerCommand, String serverUrl, String uiUrl, String traceIdFile, String traceDir) {
    try {
        String ciBin = sh(script: "which dagger-kubernetes-ci 2>/dev/null || true", returnStdout: true).trim()
        if (ciBin) {
            // Strip leading "dagger" from the command — the CI wrapper takes
            // positional args after flags and strips "dagger" itself, but we
            // pass clean args.
            String daggerArgs = daggerCommand.replaceFirst(/^dagger\s*/, '')
            assertShellSafe(serverUrl, 'serverUrl')
            assertShellSafe(uiUrl, 'uiUrl')
            sh """
                dagger-kubernetes-ci --steps --steps-format plain \
                    --server '${serverUrl}' \
                    --ui-url '${uiUrl}' \
                    --config /dev/null \
                    ${daggerArgs}
            """
        } else {
            // Fallback: run dagger directly, live. No trace discovery is
            // possible here, so the end-of-stage link points to /traces/latest.
            sh daggerCommand
        }
    } catch (e) {
        echo "[dagger-kubernetes] Pipeline failed. View: ${uiUrl}/traces/latest"
        throw e
    } finally {
        String traceId = sh(script: "cat '${traceIdFile}' 2>/dev/null || true", returnStdout: true).trim()
        sh "rm -rf '${traceDir}'"
        if (traceId) {
            echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/pipelines/${traceId}"
        } else {
            echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/traces/latest"
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

// resolveTimeoutMinutes returns a positive integer timeout in minutes, or 0
// (no timeout) when unset, zero, negative, or not a plain non-negative integer.
// A non-numeric or out-of-range value is a warning, not an error — Groovy
// `as int` would crash the build on a typo or an oversized number.
int resolveTimeoutMinutes(def value) {
    if (value == null || value == '') return 0
    String s = value.toString().trim()
    if (!(s ==~ /\d+/)) {
        echo "daggerKubernetes: ignoring non-numeric timeoutMinutes '${s}'"
        return 0
    }
    try {
        long m = s as long
        if (m <= 0) return 0
        return m > Integer.MAX_VALUE ? Integer.MAX_VALUE : (int) m
    } catch (NumberFormatException ignored) {
        echo "daggerKubernetes: ignoring out-of-range timeoutMinutes '${s}'"
        return 0
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

def provisionCli(Map params = [:]) {
    String serverUrl = params.serverUrl ?: env.DAGGER_KUBERNETES_SERVER
    String token = (params.token ?: env.DAGGER_KUBERNETES_TOKEN)?.trim()
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
    assertShellSafe(token, 'token')

    String binDir = "/tmp/dagger-cli-${env.BUILD_NUMBER}"
    assertShellSafe(binDir, 'temp dir path')

    // The Authorization header is written to a temp file and passed to curl
    // via -H @file (curl >= 7.55): the token never appears in curl's process
    // argv (readable by every local user via ps) nor in the build-wide
    // environment (CWE-214/CWE-532); it reaches the build log only through the
    // shell's xtrace of the printf line below. The file is deleted as soon as
    // provisioning finishes.
    def headerFile = "/tmp/dagger-kubernetes-auth-${env.BUILD_NUMBER}.hdr"
    assertShellSafe(headerFile, 'header file path')
    sh "printf 'Authorization: Bearer %s' '${token}' > '${headerFile}' && chmod 600 '${headerFile}'"
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

        // Provision the CI wrapper binary alongside the Dagger CLI. A missing
        // endpoint (e.g. older supervisor) is non-fatal: the wrapper is only
        // needed by pipelines that invoke dagger-kubernetes-ci themselves —
        // the dynamicStages mode runs the dagger command directly.
        String ciWrapperUrl = "${serverUrl}/api/v1/cli/ci-wrapper/latest?os=${osName}&arch=${arch}"
        int ciWrapperStatus = sh(script: "curl -fsS -w '%{http_code}' -o '${binDir}/dagger-kubernetes-ci' -H @'${headerFile}' '${ciWrapperUrl}'", returnStdout: true).trim() as int
        if (ciWrapperStatus == 200) {
            sh "chmod +x '${binDir}/dagger-kubernetes-ci'"
            echo "[dagger-kubernetes] Provisioned CI wrapper at ${binDir}"
        } else {
            echo "[dagger-kubernetes] CI wrapper not available from supervisor (status ${ciWrapperStatus}); pipelines calling the wrapper directly must use the agent's pre-installed binary"
        }
    } finally {
        sh "rm -f '${headerFile}'"
    }
    env.PATH = "${binDir}:${env.PATH}"
    echo "[dagger-kubernetes] Provisioned Dagger CLI (${version ?: 'latest'}) at ${binDir}"
}
