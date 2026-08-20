#!/usr/bin/env groovy

def call(Map params = [:], Closure body) {
    String serverUrl = params.serverUrl ?: env.DAGGER_KUBERNETES_SERVER
    String token = params.token ?: env.DAGGER_KUBERNETES_TOKEN
    String uiUrl = params.uiUrl ?: env.DAGGER_KUBERNETES_UI ?: serverUrl
    String version = params.version ?: env.DAGGER_TAG

    if (!serverUrl || !token) {
        error "daggerKubernetes: serverUrl and token are required"
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
