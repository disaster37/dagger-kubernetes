pipeline {
    environment {
        REGISTRY_CREDENTIAL = credentials('GITHUB_TOKEN')
        GIT_CREDENTIAL      = credentials('GITHUB_TOKEN')
        VERSION_TMP         = "${env.TAG_NAME == null ? "0.0.0-rc${BUILD_NUMBER}" : "${TAG_NAME.toLowerCase()}"}"
        VERSION             = "${env.CHANGE_ID ==  null ? "${VERSION_TMP}" : "0.0.0-pr${CHANGE_ID}-${BUILD_NUMBER}"}"
        BRANCH_NAME_TMP     = "${env.CHANGE_BRANCH == null ? "${GIT_BRANCH}" : "${CHANGE_BRANCH}"}"
        BRANCH_NAME         = "${env.TAG_NAME == null ? "${BRANCH_NAME_TMP}" : "main"}"
    }
    options {
        timeout time: 10, unit: 'MINUTES'
    }
    agent {
        kubernetes {
            inheritFrom 'dagger'
            defaultContainer 'dagger'
        }
    }
    stages {
        stage('ci') {
            when {
                beforeAgent true
                anyOf {
                    anyOf {
                        changeRequest target: 'main'
                        branch 'main'
                    }
                    anyOf {
                        changeRequest target: 'develop'
                        branch 'develop'
                    }
                    tag '*'
                }
            }
            steps {
                sh "dagger call -m 'github.com/disaster37/dagger-library-go/helm@v2' --src '.' 'ci' --ci 'github' --version ${VERSION} --registry-username 'env:REGISTRY_CREDENTIAL_USR' --registry-password 'env:REGISTRY_CREDENTIAL_PSW' --git-token 'env:GIT_CREDENTIAL_PSW' --git-repo-url ${GIT_URL} --git-branch ${BRANCH_NAME} export --path ."
            }
        }
    }
}
