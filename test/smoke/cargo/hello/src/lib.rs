pub fn greet() -> String {
    let mut buf = itoa::Buffer::new();
    format!("hello {}", buf.format(7))
}
