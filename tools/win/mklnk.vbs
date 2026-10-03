' 创建桌面快捷方式。COM 被安全策略拦截时会失败。
' 用法：cscript //nologo tools\mklnk.vbs <目标exe路径> [快捷方式名字]
'
' 目标路径必须由调用方给出——写死在脚本里会把作者的本机目录结构带进仓库。
Option Explicit
Dim args, exe, nm
Set args = WScript.Arguments
If args.Count < 1 Then
  WScript.Echo "用法: cscript //nologo mklnk.vbs <目标exe路径> [快捷方式名字]"
  WScript.Quit 1
End If
exe = args(0)
nm = "ModelMux"
If args.Count >= 2 Then nm = args(1)

Dim sh, fso, lnk, desktop, sc, dir, ico
Set fso = CreateObject("Scripting.FileSystemObject")
If Not fso.FileExists(exe) Then
  WScript.Echo "目标不存在: " & exe
  WScript.Quit 1
End If
Set sh = CreateObject("WScript.Shell")
desktop = sh.SpecialFolders("Desktop")
lnk = desktop & "\" & nm & ".lnk"
dir = fso.GetParentFolderName(exe)
Set sc = sh.CreateShortcut(lnk)
sc.TargetPath = exe
sc.WorkingDirectory = dir
sc.Description = nm
' 有 app.ico 就用它，没有则留空（退回 exe 自带图标）
ico = fso.BuildPath(dir, "app.ico")
If fso.FileExists(ico) Then sc.IconLocation = ico
sc.Save
WScript.Echo "OK " & lnk & " exists=" & fso.FileExists(lnk)
