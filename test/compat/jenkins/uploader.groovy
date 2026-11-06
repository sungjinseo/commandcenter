// nexus-artifact-uploader: Maven deploys (Aether) of a jar and its POM.
node {
  deleteDir()
  sh 'mkdir c && echo "hello $VERSION $BUILD_NUMBER" > c/hello.txt && jar cf hello.jar -C c .'
  writeFile file: 'hello.pom', text: """<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example.uploader</groupId>
  <artifactId>hello</artifactId>
  <version>${params.VERSION}</version>
</project>
"""
  nexusArtifactUploader nexusVersion: 'nexus3', protocol: 'http', nexusUrl: 'commandcenter:8081',
    repository: params.REPO, credentialsId: 'commandcenter',
    groupId: 'com.example.uploader', version: params.VERSION,
    artifacts: [[artifactId: 'hello', classifier: '', file: 'hello.jar', type: 'jar'],
                [artifactId: 'hello', classifier: '', file: 'hello.pom', type: 'pom']]
}
