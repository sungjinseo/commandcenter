// The build that resolves the library through the group repository.
name := "app"
crossPaths := false
autoScalaLibrary := false
resolvers += ("commandcenter" at "http://commandcenter:8081/repository/maven-public/").withAllowInsecureProtocol(true)
libraryDependencies += "com.example.sbt" % "hello" % sys.props.getOrElse("libVersion", "1.0.0")
