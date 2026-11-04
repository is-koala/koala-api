# api

HTTP sözleşmesi: OpenAPI 3.1, tek doğruluk kaynağı (ADR-0005, ADR-0006). Spec `api/openapi.yaml`'da, ortak bileşenler `api/components/*.yaml`'da durur (plan §3.1). Spec, stil rehberi, ortak bileşenler ve spec lint'i ayrı bir PR'da gelir. Sunucu arayüzü oapi-codegen ile buradan üretilir; elle yazılmış handler imzası tutulmaz.
