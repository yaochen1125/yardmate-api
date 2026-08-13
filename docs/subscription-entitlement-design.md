# 订阅资格网关校验 — 设计（未实现，待拍板后动工）

## 1. 问题

`/v1/doctor`、`/v1/identify`、`/v1/diagnose` 三个付费上游端点，服务端只验
「真设备上的真 app」（App Attest）+ 限流 + 预算桶，**不验「这个人付过钱」**。
paywall 只在客户端，改包/重放即可绕过白嫖付费上游。当前敞口被预算桶封顶
（doctor 200/h ≈ $2/h；identify/diagnose 同理），但 paywall 对懂技术者形同虚设。

## 2. 方案：StoreKit 2 签名交易的本地验签（不引入 App Store Server API）

客户端在付费端点请求带头：

```
X-Entitlement-JWS: <Transaction.jwsRepresentation of the active premium subscription>
```

服务端 `entitlement/` 新包本地验签，**零外呼**：

1. 解 JWS header 的 `x5c` 证书链 → 验到 Apple Root CA G3（证书公开，
   打包进二进制，同 attest/apple_root_ca.pem 的做法；注意 App Attest
   root 与之**不是同一张**，需另打包）
2. 叶证书须含 Apple 的 App Store receipt signing OID
3. payload 校验：`bundleId == com.chenyao.plantapp`；`productId ∈ 付费档集合`；
   `expiresDate + 宽限 > now`；`revocationDate` 缺席
4. 结果按 `X-Device-Install-Id` 缓存（TTL 24h，内存 LRU）——每请求验签
   ES256 两三次的 CPU 可忽略，缓存主要是省重复解析

失败 → `403 {"error":"subscription_required"}`（新错误码，客户端映射到 paywall）。

## 3. 灰度与回滚

vault 开关 `ENTITLEMENT_ENFORCE`: `off`（现状，默认）→ `log`（只记不拦，
观察误伤率）→ `on`。任何阶段 `off` 即回滚。先 doctor（新端点无存量客户端），
identify/diagnose 等老客户端版本覆盖率足够后再切 —— 老 app 不带头，直接
`on` 会把全部存量免费用户的 identify 打断（identify 本身不该拦，见开放问题 #3）。

## 4. 开放问题（动工前必须拍板）

1. **产品 ID 集合**：premium 的 productId 列表（月/年/终身？）— 需从
   Subscription 域确认
2. **沙盒**：staging 接受 `environment == Sandbox` 的交易，prod 拒绝
3. **identify/diagnose 是否真的要拦**：identify 有每日免费额度（客户端
   paywallReason=identifyLimit），额度逻辑在客户端 —— 服务端拦全部等于
   砍掉免费额度。可能正确形态：doctor 全拦，identify/diagnose 只做 `log`
4. **离线宽限**：JWS 里 `expiresDate` 过期但设备离线续订未同步 —— 宽限
   72h 是否够
5. **家庭共享**：签名交易天然覆盖（`ownershipType == FAMILY_SHARED`），确认放行

## 5. 明确不做

- App Store Server API 轮询/凭据（issuer key .p8）——本地验签已够，
  避免引入 Apple 侧密钥管理
- Server notifications V2（退款实时吊销）——V1 靠 24h 缓存 TTL 自然过期，
  退款者最多多用一天，成本可忽略
