// Nexus Platform plugin, tags and staging: a tag for the build, a publish with
// it, a second component tagged by search, then both moved and deleted by tag.
node {
  deleteDir()
  sh 'mkdir c && echo "hello $VERSION $BUILD_NUMBER" > c/hello.txt && jar cf hello.jar -C c .'
  def tag = "build-${params.VERSION}"
  def publish = { artifact, tagName ->
    nexusPublisher nexusInstanceId: 'commandcenter-admin', nexusRepositoryId: params.REPO, tagName: tagName,
      packages: [[$class: 'MavenPackage', mavenAssetList: [[classifier: '', extension: 'jar', filePath: 'hello.jar']],
        mavenCoordinate: [groupId: 'com.example.staging', artifactId: artifact, version: params.VERSION, packaging: 'jar']]]
  }
  createTag nexusInstanceId: 'commandcenter-admin', tagName: tag, tagAttributesJson: '{"job": "staging"}'
  publish('tagged', tag)
  publish('found', '')
  associateTag nexusInstanceId: 'commandcenter-admin', tagName: tag,
    search: [[key: 'repository', value: params.REPO], [key: 'name', value: 'found'], [key: 'version', value: params.VERSION]]
  moveComponents nexusInstanceId: 'commandcenter-admin', tagName: tag, destination: 'maven-staged'
  sh "curl -fsS -o /dev/null http://commandcenter:8081/repository/maven-staged/com/example/staging/tagged/${params.VERSION}/tagged-${params.VERSION}.jar"
  sh "curl -fsS -o /dev/null http://commandcenter:8081/repository/maven-staged/com/example/staging/found/${params.VERSION}/found-${params.VERSION}.jar"
  deleteComponents nexusInstanceId: 'commandcenter-admin', tagName: tag
}
