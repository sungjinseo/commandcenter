namespace Smoke.Hello;

public static class Greeter
{
    public static string Greet() => "hello " + Newtonsoft.Json.JsonConvert.SerializeObject(7);
}
