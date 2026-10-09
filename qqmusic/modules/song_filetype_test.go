package modules

import "testing"

// TestIsEncryptedByExtension 锁定判据：加密容器按扩展名（.mflac/.mgg/.mmp4/.mnac）判别，
// 与调用方临时拼出的枚举名无关；明文档位一律为 false。
func TestIsEncryptedByExtension(t *testing.T) {
	enc := []SongFileType{
		EncMaster, EncAtmos2, EncAtmos51, EncAtmos71, EncFLAC,
		EncOGG640, EncOGG320, EncOGG192, EncOGG96,
		// 调用方（typhoeus resolver.encType）现造的类型：名字是小写档位 id，扩展名才是权威依据。
		{Name: "master_ENC", Pref: "AIM0", Ext: ".mflac"},
		{Name: "320ogg_ENC", Pref: "O8M0", Ext: ".mgg"},
	}
	for _, ft := range enc {
		if !IsEncrypted(ft) {
			t.Errorf("%+v 应判为加密", ft)
		}
	}
	plain := []SongFileType{
		TypeMaster, TypeAtmos2, TypeAtmos51, TypeAtmos71, TypeFLAC,
		TypeOGG640, TypeOGG320, TypeOGG192, TypeOGG96,
		TypeMP3320, TypeMP3128, TypeACC192, TypeACC96, TypeACC48,
		{Name: "flac", Pref: "F000", Ext: ".flac"},
	}
	for _, ft := range plain {
		if IsEncrypted(ft) {
			t.Errorf("%+v 不应判为加密", ft)
		}
	}
}
