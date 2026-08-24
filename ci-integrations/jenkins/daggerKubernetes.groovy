#!/usr/bin/env groovy

def call(Map params = [:], Closure body) {
    String serverUrl = params.serverUrl ?: env.DAGGER_KUBERNETES_SERVER
    String token = params.token ?: env.DAGGER_KUBERNETES_TOKEN
    String uiUrl = params.uiUrl ?: env.DAGGER_KUBERNETES_UI ?: serverUrl
    String version = params.version ?: env.DAGGER_TAG

    if (!serverUrl || !token) {
        error "daggerKubernetes: serverUrl and token are required"
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

        def tempLog = File.createTempFile("dagger", ".log")
        tempLog.deleteOnExit()

        try {
            body()
        } catch (e) {
            echo "[dagger-kubernetes] Pipeline failed. View: ${uiUrl}/traces/latest"
            throw e
        }

        def logContent = tempLog.text
        def traceMatch = logContent =~ /[a-f0-9]{32,}/
        if (traceMatch) {
            def traceId = traceMatch[0]
            echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/traces/${traceId}"
        }
    }
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

    // Export the token into the environment and reference it as a shell
    // variable below (\$DAGGER_KUBERNETES_TOKEN). Groovy interpolates the
    // escaped '$' literally, so the build log records the variable NAME, not
    // the token value — never leak the credential into the console log.
    env.DAGGER_KUBERNETES_TOKEN = token

    String downloadUrl
    if (version) {
        downloadUrl = "${serverUrl}/api/v1/cli/${version}?os=${osName}&arch=${arch}"
    } else {
        String latestUrl = "${serverUrl}/api/v1/cli/versions/latest?os=${osName}&arch=${arch}"
        String latest = sh(script: "curl -fsS -H \"Authorization: Bearer \$DAGGER_KUBERNETES_TOKEN\" \"${latestUrl}\"", returnStdout: true).trim()
        def json = new groovy.json.JsonSlurper().parseText(latest)
        downloadUrl = json.url
    }

    String binDir = "${WORKSPACE}/.dagger-cli"
    sh """
        mkdir -p "${binDir}"
        curl -fsS -H "Authorization: Bearer \$DAGGER_KUBERNETES_TOKEN" "${downloadUrl}" | tar xz -C "${binDir}"
        chmod +x "${binDir}/dagger"
    """
    env.PATH = "${binDir}:${env.PATH}"
    echo "[dagger-kubernetes] Provisioned Dagger CLI (${version ?: 'latest'}) at ${binDir}"
}
