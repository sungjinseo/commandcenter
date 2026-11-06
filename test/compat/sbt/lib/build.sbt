// The library test/compat/sbt.sh publishes, Maven style: POM, jar, sources and javadoc.
organization := "com.example.sbt"
name := "hello"
version := sys.props.getOrElse("libVersion", "1.0.0")
crossPaths := false
autoScalaLibrary := false
publishMavenStyle := true
publishTo := Some(("commandcenter" at "http://commandcenter:8081/repository/" +
  (if (isSnapshot.value) "maven-snapshots" else "maven-releases") + "/").withAllowInsecureProtocol(true))
// sbt (Ivy) finds credentials by the realm of the server's challenge and the host.
credentials += Credentials(sys.props.getOrElse("realm", "commandcenter"), "commandcenter", "ci", sys.env("CC_PASSWORD"))
