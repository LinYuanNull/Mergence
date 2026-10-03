/* theme.js 首帧主题。
 *
 * 为什么必须是独立文件、且在 <head> 里同步加载：
 *
 *  1. 响应头带 `Content-Security-Policy: script-src 'self'`，**内联脚本会被
 *     直接拦掉**（不执行、不报错到页面），所以「在 head 里写一段内联脚本」
 *     这个常规做法在这里是无效的。
 *  2. 不能等 app.js：它是页面最后一个脚本，执行时浏览器已经按
 *     <html data-theme="auto"> 渲染过至少一帧。系统偏好是亮色时，
 *     那一帧就是亮色，随后才被改成深色——用户看到的就是「启动先闪一下白」。
 *  3. 不能加 defer/async：那样同样会晚于首次绘制。
 *
 * 读一次 localStorage 是微秒级开销，换掉一次可见的闪烁很划算。
 */
(function () {
  try {
    var t = localStorage.getItem('mm-theme');
    // 白名单而不是直接赋值：脏数据（手改过的 localStorage）会把 data-theme
    // 设成一个两个主题块都不匹配的值，界面会掉回「无变量」状态。
    var ok = (t === 'dark' || t === 'light') ? t : '';
    if (ok) document.documentElement.setAttribute('data-theme', ok);

    // color-scheme 决定「样式还没到时浏览器铺什么底色」：
    // 不设的话 HTML 解析期间会先铺一层白，深色主题下同样是可见的闪白。
    var sysLight = false;
    try {
      sysLight = window.matchMedia('(prefers-color-scheme: light)').matches;
    } catch (e) { /* 老内核没有 matchMedia */ }
    document.documentElement.style.colorScheme =
      ok === 'light' ? 'light' : (ok === 'dark' ? 'dark' : (sysLight ? 'light' : 'dark'));
  } catch (e) { /* 隐私模式下读不到 localStorage：保持 auto，由系统偏好决定 */ }
})();
