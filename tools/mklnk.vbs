' 创建桌面快捷方式（若 COM 被策略拦截则失败）
Option Explicit
Dim sh, fso, lnk, desktop
Set fso = CreateObject("Scripting.FileSystemObject")
Set sh = CreateObject("WScript.Shell")
desktop = sh.SpecialFolders("Desktop")
lnk = desktop & "\WorkBuddy2API.lnk"
Dim sc
Set sc = sh.CreateShortcut(lnk)
sc.TargetPath = "D:\AiWork\WorkBuddy\workbuddy-desktop\WorkBuddy2API.exe"
sc.WorkingDirectory = "D:\AiWork\WorkBuddy\workbuddy-desktop"
sc.IconLocation = "D:\AiWork\WorkBuddy\workbuddy-desktop\app.ico"
sc.Description = "WorkBuddy2API 控制台"
sc.Save
WScript.Echo "OK " & lnk & " exists=" & fso.FileExists(lnk)
