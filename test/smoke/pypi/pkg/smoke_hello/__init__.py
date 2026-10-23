import six


def greet():
    return "hello " + ("six" if six.PY3 else "?")
