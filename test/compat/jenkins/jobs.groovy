// The pipeline jobs of test/compat/jenkins.sh, one per script beside this file.
['uploader', 'publisher', 'staging'].each { name ->
  pipelineJob(name) {
    parameters {
      stringParam('VERSION', '1.0.0')
      stringParam('REPO', 'maven-releases')
      stringParam('POM', 'true', 'publisher: upload hello.pom too; else the POM is generated')
    }
    definition { cps { sandbox(true); script(new File("/work/${name}.groovy").text) } }
  }
}
