//go:build windows

package config

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// replaceFile 原子替换目标文件。杀毒软件或索引服务短暂占用时重试几次。
func replaceFile(src, dst string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 40 * time.Millisecond)
	}
	return err
}

// restrictToOwner 把配置文件的访问权限限制为当前用户（关闭继承，只授予当前用户完全控制）。
// 失败不影响主流程。
func restrictToOwner(path string) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		return
	}
	_ = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}
