import { assert, assertEquals, assertStringIncludes } from "jsr:@std/assert";

Deno.test("legacy routes", async () => {
  try {
    await import("http://localhost:8080/");
  } catch (err) {
    assertStringIncludes(err.message, "deprecated");
  }
  try {
    await import("http://localhost:8080/v135");
  } catch (err) {
    assertStringIncludes(err.message, "deprecated");
  }
  {
    const { esm, build, transform } = await import("http://localhost:8080/build");
    assertEquals(typeof esm, "function");
    assertEquals(typeof build, "function");
    assertEquals(typeof transform, "function");
    try {
      esm``;
    } catch (err) {
      assertStringIncludes(err.message, "deprecated");
    }
  }
  {
    const { esm, build, transform } = await import("http://localhost:8080/v135/build");
    assertEquals(typeof esm, "function");
    assertEquals(typeof build, "function");
    assertEquals(typeof transform, "function");
    try {
      esm``;
    } catch (err) {
      assertStringIncludes(err.message, "deprecated");
    }
  }
  {
    const res = await fetch("http://localhost:8080/react-dom@18.3.1?pin=v135", {
      headers: {
        "User-Agent":
          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
      },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertEquals(res.headers.get("X-Esm-Id"), "v135/react-dom@18.3.1/es2022/react-dom.mjs");
    assertEquals(res.headers.get("X-TypeScript-Types"), "http://localhost:8080/v135/@types/react-dom@~18.3/index.d.ts");
    assertStringIncludes(await res.text(), "/v135/react-dom@18.3.1/es2022/react-dom.mjs");
  }
  {
    const res = await fetch("http://localhost:8080/react-dom@18.3.1?pin=v135&dev", {
      headers: {
        "User-Agent":
          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
      },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertEquals(res.headers.get("X-Esm-Id"), "v135/react-dom@18.3.1/es2022/react-dom.development.mjs");
    assertEquals(res.headers.get("X-TypeScript-Types"), "http://localhost:8080/v135/@types/react-dom@~18.3/index.d.ts");
    assertStringIncludes(await res.text(), "/v135/react-dom@18.3.1/es2022/react-dom.development.mjs");
  }
  {
    const res = await fetch("http://localhost:8080/react-dom@18.3.1&pin=v135&dev", {
      headers: {
        "User-Agent":
          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
      },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertEquals(res.headers.get("X-Esm-Id"), "v135/react-dom@18.3.1/es2022/react-dom.development.mjs");
    assertEquals(res.headers.get("X-TypeScript-Types"), "http://localhost:8080/v135/@types/react-dom@~18.3/index.d.ts");
    assertStringIncludes(await res.text(), "/v135/react-dom@18.3.1/es2022/react-dom.development.mjs");
  }
  {
    const res = await fetch("http://localhost:8080/react-dom@18.3.1/client?pin=v135", {
      headers: {
        "User-Agent":
          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
      },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertEquals(res.headers.get("X-Esm-Id"), "v135/react-dom@18.3.1/es2022/client.js");
    assertEquals(res.headers.get("X-TypeScript-Types"), "http://localhost:8080/v135/@types/react-dom@~18.3/client~.d.ts");
    assertStringIncludes(await res.text(), "/v135/react-dom@18.3.1/es2022/client.js");
  }
  {
    const res = await fetch("http://localhost:8080/stable/react@18.3.1", {
      headers: {
        "User-Agent":
          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
      },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertEquals(res.headers.get("X-Esm-Id"), "stable/react@18.3.1/es2022/react.mjs");
    assertEquals(res.headers.get("X-TypeScript-Types"), "http://localhost:8080/v128/@types/react@~18.3/index.d.ts");
    assertStringIncludes(await res.text(), "/stable/react@18.3.1/es2022/react.mjs");
  }
  {
    const res = await fetch("http://localhost:8080/v135/react-dom@18.3.1", {
      headers: {
        "User-Agent":
          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
      },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertEquals(res.headers.get("X-Esm-Id"), "v135/react-dom@18.3.1/es2022/react-dom.mjs");
    assertEquals(res.headers.get("X-TypeScript-Types"), "http://localhost:8080/v135/@types/react-dom@~18.3/index.d.ts");
    assertStringIncludes(await res.text(), "/v135/react-dom@18.3.1/es2022/react-dom.mjs");
  }
  {
    const res = await fetch("http://localhost:8080/v135/@types/react-dom@~18.3/index.d.ts", {
      redirect: "manual",
    });
    res.body?.cancel();
    assertEquals(res.status, 302);
    assert(/^http:\/\/localhost:8080\/v135\/@types\/react-dom@18\.3\.\d\/index\.d\.ts$/.test(res.headers.get("Location")!));
  }
  {
    const res = await fetch("http://localhost:8080/v135/@types/react-dom@18.3.1/index.d.ts");
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/typescript; charset=utf-8");
    assertStringIncludes(await res.text(), "https://esm.sh/v135/@types/react@18.");
  }
  {
    const res = await fetch("http://localhost:8080/v135/@types/react-dom@~18.3/client~.d.ts", {
      redirect: "manual",
    });
    res.body?.cancel();
    assertEquals(res.status, 302);
    assert(/^http:\/\/localhost:8080\/v135\/@types\/react-dom@18\.3\.\d\/client~\.d\.ts$/.test(res.headers.get("Location")!));
  }
  {
    const res = await fetch("http://localhost:8080/v135/@types/react-dom@18.3.1/client~.d.ts");
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/typescript; charset=utf-8");
    assertStringIncludes(await res.text(), "createRoot");
  }
  {
    const res = await fetch("http://localhost:8080/stable/react@18.3.1/es2022/react.mjs", {
      headers: { "User-Agent": "i'm a browser" },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertStringIncludes(await res.text(), "createElement");
  }
  {
    const res = await fetch("http://localhost:8080/v135/react-dom@18.3.1/es2022/client.js", {
      headers: { "User-Agent": "i'm a browser" },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertStringIncludes(await res.text(), "createRoot");
  }
  {
    const res = await fetch("http://localhost:8080/v64/many-keys-weakmap@1.0.0/es2022/many-keys-weakmap.js", {
      headers: { "User-Agent": "i'm a browser" },
    });
    assertEquals(res.status, 200);
    assertEquals(res.headers.get("Content-Type"), "application/javascript; charset=utf-8");
    assertStringIncludes(await res.text(), "ManyKeysWeakMap");
  }
  {
    const res = await fetch("http://localhost:8080/v136/react-dom@18.3.1/es2022/client.js", {
      headers: { "User-Agent": "i'm a browser" },
    });
    res.body?.cancel();
    assertEquals(res.status, 400);
  }
  {
    const res = await fetch("http://localhost:8080/react-dom@18.3.1/es2022/client.js", {
      headers: { "User-Agent": "i'm a browser" },
    });
    res.body?.cancel();
    assertEquals(res.status, 404);
  }
});