// Nexus Platform plugin: nexusPublisher posts the files to the component upload API.
node {
  deleteDir()
  sh 'mkdir c && echo "hello $VERSION $BUILD_NUMBER" > c/hello.txt && jar cf hello.jar -C c . && cp hello.jar hello-sources.jar'
  def artifact = params.POM == 'true' ? 'hello' : 'hello-nopom'
  writeFile file: 'hello.pom', text: """<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example.publisher</groupId>
  <artifactId>${artifact}</artifactId>
  <version>${params.VERSION}</version>
</project>
"""
  def assets = [[classifier: '', extension: 'jar', filePath: 'hello.jar'],
                [classifier: 'sources', extension: 'jar', filePath: 'hello-sources.jar']]
  if (params.POM == 'true') {
    assets << [classifier: '', extension: 'pom', filePath: 'hello.pom']
  }
  nexusPublisher nexusInstanceId: 'commandcenter', nexusRepositoryId: params.REPO,
    packages: [[$class: 'MavenPackage', mavenAssetList: assets,
      mavenCoordinate: [groupId: 'com.example.publisher', artifactId: artifact, version: params.VERSION, packaging: 'jar']]]
}
