package payments

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

const Version = 2

var PaymentChannelUniversalCodeBOCs = []string{
	"b5ee9c7241024f01001164000114ff00f4a413f4bcf2c80b010201200231020148032f0202cc041a020120051003f7d7c48f92076a26869006900699fe99fe98fe9ffe9ffe9bfea7a027a026885eb9610fecf8d1a71816b9612867b6a3271816b9613808c43de470e6a6a7c49086f0866885e0855884d0844883c0833882b0822f802af85f06b96119ae4fd1e470e6a6a7c49086f0866885e0855884d0844883c0833882b0822f8032f85c06070801fe8308d718f8922d6ef2d06b21f901547d78e3044140f910f2e0658210048b33af25f007d4d4d74c0ed025d001d3000199d33fd3ffd3ff810099966d6d6d580370e201d31fd200d200d70bff08d31fd70b1f5242a0a0f823b9f2e08027c300f2e08e23c000f2d08d5087f00a111423f00a28f9008b08511011178307f42e6fa14804f0d4d4f8920cf2d06420f90123d05461a1f9109722d028f910c300923070e2f2e065c8cf92867b6a3213cccc21cf16c90182102aa9380625f007d33fd70b3f511abe9428bec300923070e2f2e0696d6d6d6d6d6d6d70296ee3002ad0f8002ad0fa40d111135613c705e30f7f547907547987547987561156143738393f033ee0d72c20bb27e594e302d72c21fd1a4e84e302d72c212a19548c31e3025f0b090d0f01fcd31fd3000196d4d74c81009594306d6d70e2f8922df2e07525d0fa40d121c705f2e0656d6d6d6d6d7056156e9257158e4c5f060fd0d3000199d33fd3ffd3ff810099966d6d6d580370e201d31fd200d20031d3ff31d181009b2cd0d70b1f23a0f823bcf2e06f27c000f2d06a532ab9923a199132e20111140115144330e20a04fc5614c000b3996c416d6d6d58037001df1114c000b301111470e304707007c0009237378f4e6c225436acdb3c5438dedb3c0311150302111402265446305446e0561801561851101111f0095613925b2f93373f20e21113c00092327f9756125003bcc300e2f2e0698100991112401f50ee0605e2c8cf850814ce89cf16c942420b0c00330000000000000000000000000001aa64edb6000000000000000102468042fb004576041111040301111101db3c54798754798754798756132adb3c6cb1ed544d4e01fed70bfff8922c6ef2d06b22d0fa40d121c705f2e0650cd023d001d3000199d33fd3ffd3ff810099966d6d6d580370e201d31fd200d200d70bff08d31fd70b1f5242a020f823b9f2e06ea0f823bcf2e07007c000f2e070c8cf850801111301ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c90e024c8042fb001047103655220211110201111101db3c54798754798754798756132adb3c6cb1ed544d4e01f0f8922b6ef2d06b2bd023d001d300019cd33f31d3ff31d3ff318100999170e201d70b1f02d31fd31fd70b1f03c000993031a0f823b9f2e0719b03a058a0a0f823b9f2e071e2f800546aa0546aa0546aa0546aa0546aa0561501f00d303839706d07a406a45479165473875398561256115611db3c6cb1ed544e02012011150101581202f621f901c8cf93808c43de25cf1424cf1423cf16c905d05461c1f9109903d0541309f910c30093303270e2f2e0658210c2c8e57927f007d33fd70b3f53c1bb9553b0bbc3009170e2f2e06c51c1bd923a7f9551abbdc300e2f2e06c5479a26e923b3be30ef8002c544c302c544c302c544c302c544c302c544c302c021314005c24d0d3000196d70b3f81009993306d70e2c0009530343a3a6d9f544fede304500cbc926d33de0a4919e2109a40190124f00b30547a98547a98547a9853a9db3ced544e02012016190101201701f421f901c8cf919ae4fd1e25cf1424cf1423cf16c905d05461c1f9109903d0541309f910c30093303270e2f2e06582103e72726c27f007d33fd70b3f51c1bb9551abbbc300923a70e2f2e06cf8002c544c302b544d302c544c302c544c302c544c302c02f00c5b38706d07a408a4547a18547398547a985612561118010cdb3ced5450974e0101201e0201201b240201201c210201201d1f02f3208408b5869a4976cf348034c7f4c7fd01544faebcb8210a6ebcb8217e08ef3cb8223e0001e9151eea551ece551eea54eeb6cf3b553e03c9dba44de38edc0a35ce4c238a4835d2f000bcb824f00a3cb824f5cb081d8790db3cb824f535c2c1dcac3cb82275ce4c00690871c004b98c2101eefcb824c1fb5578a01e4e002002d31fd37f5023baf2e06858baf2e0720101202000e204d0d33fd3ffd3ffd106d0d33fd3ffd3ffd153bdbe95537cbec3009170e295524dbec300923c70e295521dbec300923c70e2955052bac30093323470e29417bac30093303670e2955052bac30093323470e29404bac30093313370e29402bac300936c2170e293bac300925b70e2f2e069020120222601012023003201d739f2e073d30701c003f2e07320d70bff58baf2e073d74c020120252a02012026280101202700ac326c8402d0fa40d15122c7058e1b02d0d35f31fa0030c8cf858812ce01fa0271cf0b6accc970fb007fe13031c8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00700101202900b4316c63333302d0fa40d166c7058e1f01d0d35f31fa0030c8cf858812ce01fa02821025432a91cf0b8ac970fb007fe131c8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00700201202b2d0101202c00e8356c773704d0fa40d15122c7058e3704d0d35f31fa0030c8cf905d93f2ca15cb1f02c000943431cf819701cf8312cc13cce2c9c8cf858813ce01fa0271cf0b6accc970fb007fe1155f05c8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00700101202e00b8326c8402d0fa40d15122c7058e2102d0d35f31fa0030c8cf858812ce01fa0282103fa349d0cf0b8acbffc970fb007fe13031c8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb007001d5a1432bdb45dbf7da89a1a40063a401a67e63a67e63a63e63a7fe63a7fe63a6fe63a9e80863e809a20524b6e1c242dd24b6e3c003a003a003a6000339a67e63a7fe63a7fe6302013322e1c403ae163e05a63fa63fae163ea68541f0477926be0ae5c007800124be09c61ceb3000385331a021a0f823bc955f0473db31e003a058a0a0f823bc9374db31e004f4f2ed44d0d200d200d33fd33fd31fd3ffd3ffd37fd4f404f404d10bd72c25443dc7dc8e4029942b6ec3009170e2f2e065d4d420f90103d0546391f9109901d0541206f910c300936c2170e2f2e08710ab109a1089107810671056104510344130f0085f0be0d72c218d20b464e302d72c27011887bce30289d72732333435005a29f2d0658308d71820f901547c76e3044130f910f2e08710ab109a1089107810671056104510344130f0085f0b0038d4d48b0210de10cd10bc10ab109a10891078106710561045f0055f0b0008a19eda8c0482e302d72c2335c9fa3c8e1cd4d48b0210de10cd10bc10ab109a10891078106710561045f0065f0be0d72c25e8e6dedce302d72c212af085b4e302d72c21fd9f1a343640444604f0d4d48b020cf2d06420f90123d05461a1f9109722d028f910c300923070e2f2e065c8cf92867b6a3213cccc21cf16c90182102aa9380625f007d33fd70b3f511abe9428bec300923070e2f2e0696d6d6d6d6d6d6d70296ee3002ad0f8002ad0fa40d111135613c705e30f7f547907547987547987561156143738393f00be10895f0929b36d6d5477652770f82a08c8ca0070cf0ba015cbff13cbffcb7fcc13f400f400c98100816d6d2481008553755cf90001f9005ad76501d765c8cf8c0804d2cb0fcb0fcbffcbff71f90400c8cf8a0040cbffc9d0c8cec90908555000585f0ac8cf850819ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00014ad35f31fa0030aa0001c0008e166c71c8cf85881bce500afa0271cf0b6a19ccc970fb00e30e3a04fe57118100815004ba8100825004bac8cf850917ca07238e4e268e285472545cf90001f9005ad76501d76524aa09820a0381a0a0c8cb1fcb0fcb0fcbffcbff71f90400318e2053545cf90001f9005ad76501d765c8cf8c0804d2cb0fcb0fcbffcbff71f90400e29324f900e2279332cbffe30d500ffa0270cf0b6801e30f19cc3b3c3d3e002c830724a1aea5b0830724a103800b25d72458ce58cf010030039a02cf86c01ccb04cf85b0963c01cf86361be21bcc1acc0010323c31cf87c01acc0008c970fb00010cdb3c6cb1ed544e01fe8308d7182af2e07520f901547c76e3044130f910f2e0658210a5af1d0724f007d3000196d4d74c81009594306d6d70e26d6d6d6d6d7056136e8e3f5f062dd0d3000199d33fd3ffd3ff810099966d6d6d580370e201d31fd200d20031d3ff31d181009b2bd0d70b1f23a0f823bcf2e06f01f2d06a26c000f2d06adf20c000b34103fc9a6c22326d336d6d5a7002dfc000b39330f823df7f707028c0008f5435355478bddb3c53bf5612db3c031118030211170227544730544770561b01561b54200af0095616946c215612983057122101111201e205c00092377f955248bcc300e2f2e06911111110103605103481009904dff8008b02561505561505561505424243006602d08308d718d4d120f9004004f910f2e06501d0d31fd37fd33fd3ffd3ffd4d105821098b70b88baf2e0685035baf2e0724133028a561505561505561505561505561505561505561505041120040356200302111602011115011114f00e3055330311110358db3c54798754798754798756132adb3c6cb1ed544d4e01fe8308d7182c6ef2d06b20f901547c76e3044130f910f2e06582108a3056b724f007d31ff404d4d74c5139baf2e0852dd025d001d3000199d33fd3ffd3ff810099966d6d6d580370e201d31fd200d200d70bff08d31fd70b1f5242a020f823b9f2e06ea0f823bc9420b3c3009170e2f2e07023c000f2d08d5085f00a5093f00a4503f2f8000ea456120156120156120156125110561201561201561201561201561201111ddb3ced54f80f8e2b068020f4966fa5208e1b8b0840058020f42e6fa1f2e073ed1e0211100201111001da214e1e926c21e2b317e636d7680cd7681027106c0504103c401cdb3c5479875479d754798753e9db3c6cb1ed544e4d4e032ce302d72c212a19548ce302d72c25c8b5e3e4e302f23f474a4b01fe8308d7188b022d6ef2d06b21f901547d78e3044140f910f2e0658210048b33af25f007d4d4d74c0ed025d001d3000199d33fd3ffd3ff810099966d6d6d580370e201d31fd200d200d70bff08d31fd70b1f5242a0a0f823b9f2e08027c300f2e08e23c000f2d08d5087f00a111423f00a28f9008b08511011178307f42e6fa14802fa01111701038307f40e6fa1039302c300923270e2975615c700b3c3009170e2f2e08c09d0ed1ef8005614b30311160301da3006d7688b0228c70591378e2ac8cf850818ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00e2471550640311110302111101db3c5479875479874d49011854798756132adb3c6cb1ed544e01f2308b022b6ef2d06b2bd023d001d300019cd33f31d3ff31d3ff318100999170e201d70b1f02d31fd31fd70b1f03c000993031a0f823b9f2e0719b03a058a0a0f823b9f2e071e2f800546aa0546aa0546aa0546aa0546aa0561501f00d303839706d07a406a45479165473875398561256115611db3c6cb1ed544e01fe8308d7182c6ef2d06b20f901547c76e3044130f910f2e06582101c1b99b824f007d31fd70bff5117baf2e0852bd023d001d3000199d33fd3ffd3ff810099966d6d6d580370e201d31fd200d200d70bff08d31fd70b1f5242a020f823b9f2e06ea0f823bc9420b3c3009170e2f2e07023c000f2d08d5184baf2e06907f2d0644c0296f8008b0256110256110256110256110256110256110256110256110256110256110201111c012df00f307f0ca4104710361025111143431cdb3c5479875479d754798753e9db3c6cb1ed544d4e004cc805c0009635353501cf819f04cf8317cb3f15cbff13cbff103412e2cb1f13ca00ca00cbffc900380ac8ca0019ca0017cb3f15cb3f13cb1fcbffcbffcb7fccf400f400c9247ed659",
}

// PaymentChannelCodeBoCs https://github.com/xssnick/payment-channel-contract/blob/master/contracts/payment_channel.tolk
var PaymentChannelCodeBoCs = []string{ // the newest version must be first
	"b5ee9c7241023601000ccc000114ff00f4a413f4bcf2c80b0102016202330202cc03240257df6d176fdfc48f92076a26869006a69ffe9ffe9bfea699fe99ffa026a68856b961164f8e24e7187fc4978064040601ca2ad001d70a0001fa0031fa4031fa4031d72c0596d70b1f81008c8e15d72c0795d74c81008d9ad72c023192f23fe16d70e2e281008cba8e2b6ef2e078f8978209c9c380bef2e077f8978209c9c380a110ab109a1089107810671056104510344300f008e30d05006cf89782097d7840bef2e077f8988020f4666fa194016ec300923170e2f2e079fa043010ab109a1089107810671056104510344300f0080394d72c239b1684e48e22d33ffa00fa40f892f89710ef10de10cd10bc10ab109a1089107810675531f00adb31e0d72c268b9aa004e302d72c203b5fef8ce30f08091067105610451034102307080c01ead33f31fa4030f8920bd0fa00fa40fa40d72c0596d70b1f81008c8e15d72c0795d74c81008d9ad72c023192f23fe16d70e2e281008dbaf2e078d0fa40fa4030d72c0131f2e07c520fc705f2e0650dc8ce13cec9c858fa0212ce1bcecf87801accc91089107810671056104510344130db3ced54db312701fed4d4f8978209c9c380a121f9010282104a390cac2af00d04d05462c1f9109702d029f910c300936c2170e2f2e06501fa00fa00d33fd33ffa00fa00300ed0fa00fa00fa00fa003051c6b951b5b91bb0f2e06c5474386e9235358e202ad0d33ffa0031d3ff31d70b3f08b99335357f975056bcc3001045e2926d39dee253faa10903fc20c2fff2e07f5612d06d01fa00fa40fa40d72c0596d70b1f81008c8e15d72c0795d74c81008d9ad72c023192f23fe16d70e2e226c2008eab355710c8cf928cbc2cf25612cf0b7f7024544430240256145449305447c052c0db3c1da10f11140f0c103493365715e2537ea120c2fff2e07f20c200945f06323ee30d05c2ff1f0a0b014c3f236e9e33c8cf928cbc2cf25611cf0b7f03de2110375056111550341f70db3c16a1107d05071f018cf2e07753c2a05347a0a1c2ff9b5343a05338a0a1c2ffc3009170e2f2e076c8500dfa025004fa025005fa025005fa025004fa0258fa02c95478065478765478d75612db3ced54270216d72c23cd74cdace30f50070d1001fed2008308d718547ba9547ba9547ba9561509f2d06429f901543c76e3044cb0f910f2e065078210481ebc4423f00d3024d026d001fa00fa00fa00fa00fa00fa0030b15004b158b1b1b1c000f2e066820898968001fa00fa4031fa4031d72c0596d70b1f81008c8e15d72c0795d74c81008d9ad72c023192f23fe16d70e2e2200e01fec000915b8e603381008d5003ba8e4d01d0fa40fa40d182100bebc20001d72c01318e368209c9c380f828c8cf850814ce01fa028d06400000000000000000000000000163b5cb980000000000000004cf1612cecf81c970fb009131e297318210042c1d80e201e25202bc97f8276f10bbc300923070e2f2e07b7f09105810470f0114103640550403db3ced54270324d72c26958f775ce302d72c240baf0aece30f11131502fe313807d4d420f9010182108243e9a327f00d03d0546191f9109701d026f910c300925b70e2f2e065fa00fa00d33fd70b3f29d03a09fa00fa00fa00fa00305174b99551cbb9c300923c70e2f2e06cc8cf93777222ea28cf0b7f2dd0fa00fa40fa40d72c058e15d72c0795d74c81008d9ad72c023192f23fe16d70e2e30d516a1c120390a0529ca01ba1722454443024544e302a54473052b0db3c305057a0507ea01da122060743140d810082db3c3070c8cf8c000002c96d547216547876547ed65612db3c6ca1ed54db311f1f2703fed2008308d718547ba9547ba9547ba95615016ef2e06a29f901547c76e30441c0f910f2e0650882108c62369224f00dd4d74c543146db3c30543589db3c30538abe255613beb05356beb05392beb0524bbe1ab0527abe19b0f2e06923c20096513fbef2e0699133e226c200965167bef2e0699136e2104610354413f82350031717140228111070db3c1069105810471036450402db3ced5422270212d72c24d3be06dce30f161a03fed2008308d718547ba9547ba9547ba95615216ef2d06b2af901547d87e30441d0f910f2e065098210b8a2137925f00dd4d74c543157db3c3054359adb3c30538bbe535bbeb05356beb05392beb0524bbe1ab0527abe19b0f2e06923c200965138bef2e0699133e226c200965168bef2e0699136e20ed028d001d33ffa00d3ff171718009802d08308d718d4d120f9004004f910f2e06501d0d31fd37fd33ffa00d3fff404d105821043685374baf2e0685035baf2e072705470036e91359d5f0302d0d33ffa00d3ffd14444e25e22550202fcd33ffa00d3ffd31fd200d70a0009d31ffa00305232a0f823bcf2e06f09f2d07d208e1235383b57135284bc92317f955262bcc300e28e1d323939395612b992377f955288bcc300e2081111081048476010251024e2215614bdf2e06d9a9204a0940fa00e03e203923031e2104710364514103e11107fdb3c106910581047221901121036450402db3ced542702fcd72c22b61cda648e8f323831d72c212a19548c31e302f23fe1d2008308d718547ba9547ba9547ba95615216ef2d06b2af901547d87e30441d0f910f2e06509821014588aab25f00df404d74c0ad024d001d33ffa00d3ffd33ffa00d3ffd31fd200d70a0009d31ffa0031d70b1f5232a020f823b9f2e06ea0f823bcf2e0701b2103fc256ef2d06b25d03620d006d33ffa00d3ff31d33ffa00d3ff31d70b1f0ad31ffa0031d70b1f0ba0500aa0f823b9f2e07127d03807fa00fa00fa00fa00305324a053c1a0a1534ca05363a0a121c1009306a2059131e220c100931ca10b9130e205a40aa4c8cf93777222ea28cf0b7f2dd0fa00fa40fa40d72c05e30f518aa01c1d1e000cd70b1f81008c002ad72c0795d74c81008d9ad72c023192f23fe16d70e203965611500ca01ba1722454443024544e302c54473052b0db3c30507fa05074a013a1240607105d044d13810082db3c3070c8cf8c000002c96d547216547876547de65612db3c6ca1ed54db311f1f2701c23636367022c0008e146c22c8cf850812ce5004fa0270cf0b6a12cf13c98ebe3081008d58ba8e336d25c2009c8020c85007fa06431306f443923234e28209c9c380c8cf850815ce5004fa0213f40070cf0b69cf13c98209c9c380e30d59e201fb002000a201d0fa4031fa4030821004c4b4006d8209c9c380c88bc0f8a7ea500000000000000008cf165008fa0224cf1614ce13f4005005fa02cf8113cf13c9c8cf850814ce58fa0271cf0b6a12ccc9821004c4b40002fe56149354743293547765e21115d739f2e073d30701c003f2e07320d70bff011116baf2e0731114d74c8e2a0b8020f4966fa5208e1a8b08400f8020f42e6fa1f2e073ed1e12da1101111601a011150c926c21e2b31ce63b0ad7681115936c22329c353535041111040f50634440e21048103710261025104f0311110302db3c2223003408c8cb3f5007fa0215cbff13cb3f01fa02cbffcb1fca00ca00c90120106910581047103645044313db3ced5427020120252d0201202628016542bf2e075236ef2e06a0ad0fa00fa000c9202a09358a001e2c801fa0201fa0219cec95479075479875479875611db3ced5408827003009c8ca0018cc16cbff14cbff12cb7fcccb3fcb3ff400ccc9020120292b01dd23b9fb68bb7eccc9743e800c7e900c7e900c75cb0165b5c2c7e04023238575cb01e575d320402366b5cb008c64bc8ff85b5c38b8a040236ebcb81e3400f4800064f5d3343780fe900c7e900c0120828583b02e648cdfe5d44d31c16cf0c038a5cc5b24ccd67c02f8c3a0041ff6ce202a0072543332945b02f00bede3ba737fed118e21d72c22c9f1c49cf2bfd70a0010bc10ac1c1918171615144330f0085509f00cdb31ed41edf101f2ff0101202c00706d71c8c88bc0f8a7ea500000000000000008cf165005fa0225cf1615cef4005003fa02cf81cf13c9c8cf850812ce71cf0b6eccc98042fb000201482e310101202f01f6508a5f08d0fa00fa4031fa4031d72c05943081008c8e13d72c07943081008d99d72c023192f23fe170e2e2c0008e1e01d0fa00fa00fa00fa00fa00fa00305155a05214a013a15aa05aa0a1a0a09131e272fb02c8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c9810082300004fb0001012032002002d31fd37f5023baf2e06858baf2e072020120343500c1bc517f6a26869006a18e9ff98e9ff98e9bf98ea699f98e99f98fa026a18e881492db87090b7492db8f000e800e800e99f98fd0018c080a06b90fd0018e9ff98eb858f80e98ffd0018eb858fa990d07c11de49af81b9700150507c11de48b9f03a400c9bfd7476a268690018ea69ffe9ffe9bfea699fe99ffa026a6884680468047d007d007d007d007d007d001806fd007d207d201841008517f6a1ec142b08d029abd050a943500888935000888880d08202881c23b037832854b781282a378120a437818aa33047974b95c",
}

var PaymentChannelCodes = func() []*cell.Cell {
	var codes []*cell.Cell
	for _, c := range PaymentChannelCodeBoCs {
		codeBoC, _ := hex.DecodeString(c)
		code, _ := cell.FromBOC(codeBoC)
		codes = append(codes, code)
	}
	return codes
}()

var PaymentChannelUniversalCodes = func() []*cell.Cell {
	var codes []*cell.Cell
	for _, c := range PaymentChannelUniversalCodeBOCs {
		codeBoC, _ := hex.DecodeString(c)
		code, _ := cell.FromBOC(codeBoC)
		codes = append(codes, code)
	}
	return codes
}()

func init() {
	tlb.Register(CurrencyConfigJetton{})
	tlb.Register(CurrencyConfigEC{})
	tlb.Register(CurrencyConfigTon{})
}

type Signature struct {
	Value []byte `tlb:"bits 512"`
}

type ClosingConfig struct {
	QuarantineDuration       uint32    `tlb:"## 32"`
	MisbehaviorFine          tlb.Coins `tlb:"."`
	ConditionalCloseDuration uint32    `tlb:"## 32"`
}

type ConditionalPayment struct {
	Amount    tlb.Coins  `tlb:"."`
	Condition *cell.Cell `tlb:"."`
}

type SemiChannelBody struct {
	Seqno            uint64    `tlb:"## 64"`
	Sent             tlb.Coins `tlb:"."`
	ConditionalsHash []byte    `tlb:"bits 256"`
}

type SemiChannel struct {
	_                tlb.Magic        `tlb:"#43685374"`
	ChannelID        ChannelID        `tlb:"bits 128"`
	Data             SemiChannelBody  `tlb:"."`
	CounterpartyData *SemiChannelBody `tlb:"maybe ^"`
}

type SignedSemiChannel struct {
	Signature Signature   `tlb:"."`
	State     SemiChannel `tlb:"^"`
}

type QuarantinedState struct {
	StateA            SemiChannelBody `tlb:"."`
	StateB            SemiChannelBody `tlb:"."`
	QuarantineStarts  uint32          `tlb:"## 32"`
	StateCommittedByA bool            `tlb:"bool"`
	StateChallenged   bool            `tlb:"bool"`
}

type CurrencyConfigEC struct {
	_  tlb.Magic `tlb:"$10"`
	ID uint32    `tlb:"## 32"`
}

type CurrencyConfigTon struct {
	_ tlb.Magic `tlb:"$0"`
}

type CurrencyConfigJettonInfo struct {
	Master *address.Address `tlb:"addr"`
	Wallet *address.Address `tlb:"addr"`
}

type CurrencyConfigJetton struct {
	_    tlb.Magic                `tlb:"$11"`
	Info CurrencyConfigJettonInfo `tlb:"^"`
}

type PaymentConfig struct {
	StorageFee     tlb.Coins        `tlb:"."`
	DestA          *address.Address `tlb:"addr"`
	DestB          *address.Address `tlb:"addr"`
	CurrencyConfig any              `tlb:"[CurrencyConfigTon,CurrencyConfigJetton,CurrencyConfigEC]"`
}

type Balance struct {
	DepositA  tlb.Coins `tlb:"."`
	DepositB  tlb.Coins `tlb:"."`
	WithdrawA tlb.Coins `tlb:"."`
	WithdrawB tlb.Coins `tlb:"."`
	SentA     tlb.Coins `tlb:"."`
	SentB     tlb.Coins `tlb:"."`
}

type AsyncChannelStorageData struct {
	Initialized     bool              `tlb:"bool"`
	Balance         Balance           `tlb:"^"`
	KeyA            []byte            `tlb:"bits 256"`
	KeyB            []byte            `tlb:"bits 256"`
	ChannelID       ChannelID         `tlb:"bits 128"`
	ClosingConfig   ClosingConfig     `tlb:"^"`
	CommittedSeqnoA uint64            `tlb:"## 64"`
	CommittedSeqnoB uint64            `tlb:"## 64"`
	Quarantine      *QuarantinedState `tlb:"maybe ^"`
	PaymentConfig   PaymentConfig     `tlb:"^"`
}

type OpenConfigContainer struct {
	KeyA          []byte        `tlb:"bits 256"`
	KeyB          []byte        `tlb:"bits 256"`
	ChannelID     ChannelID     `tlb:"bits 128"`
	ClosingConfig ClosingConfig `tlb:"^"`
	PaymentConfig PaymentConfig `tlb:"^"`
}

/// Messages

type InitChannel struct {
	_         tlb.Magic `tlb:"#79ae99b5"`
	IsA       bool      `tlb:"bool"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_         tlb.Magic `tlb:"#481ebc44"`
		ChannelID ChannelID `tlb:"bits 128"`
	} `tlb:"."`
}

type TopupBalance struct {
	_   tlb.Magic `tlb:"#593e3893"`
	IsA bool      `tlb:"bool"`
}

type CooperativeClose struct {
	_          tlb.Magic `tlb:"#d2b1eeeb"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#8243e9a3"`
		ChannelID ChannelID `tlb:"bits 128"`
		SentA     tlb.Coins `tlb:"."`
		SentB     tlb.Coins `tlb:"."`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
	} `tlb:"."`
}

type CooperativeCommit struct {
	_          tlb.Magic `tlb:"#076bfdf1"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#4a390cac"`
		ChannelID ChannelID `tlb:"bits 128"`
		SentA     tlb.Coins `tlb:"."`
		SentB     tlb.Coins `tlb:"."`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
		WithdrawA tlb.Coins `tlb:"."`
		WithdrawB tlb.Coins `tlb:"."`
	} `tlb:"."`
}

type StartUncooperativeClose struct {
	_           tlb.Magic `tlb:"#8175e15d"`
	IsSignedByA bool      `tlb:"bool"`
	Signature   Signature `tlb:"."`
	Signed      struct {
		_         tlb.Magic         `tlb:"#8c623692"`
		ChannelID ChannelID         `tlb:"bits 128"`
		A         SignedSemiChannel `tlb:"^"`
		B         SignedSemiChannel `tlb:"^"`
	} `tlb:"."`
}

type ChallengeQuarantinedState struct {
	_               tlb.Magic `tlb:"#9a77c0db"`
	IsChallengedByA bool      `tlb:"bool"`
	Signature       Signature `tlb:"."`
	Signed          struct {
		_         tlb.Magic         `tlb:"#b8a21379"`
		ChannelID ChannelID         `tlb:"bits 128"`
		A         SignedSemiChannel `tlb:"^"`
		B         SignedSemiChannel `tlb:"^"`
	} `tlb:"."`
}

type SettleConditionals struct {
	_         tlb.Magic `tlb:"#56c39b4c"`
	IsFromA   bool      `tlb:"bool"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                    tlb.Magic        `tlb:"#14588aab"`
		ChannelID            ChannelID        `tlb:"bits 128"`
		ConditionalsToSettle *cell.Dictionary `tlb:"dict 32"`
		ConditionalsProof    *cell.Cell       `tlb:"^"`
	} `tlb:"."`
}

type FinishUncooperativeClose struct {
	_ tlb.Magic `tlb:"#25432a91"`
}

func (s *SignedSemiChannel) Verify(key ed25519.PublicKey) error {
	if bytes.Equal(s.Signature.Value, make([]byte, 64)) &&
		s.State.Data.Sent.Nano().Sign() == 0 &&
		bytes.Equal(s.State.Data.ConditionalsHash, make([]byte, 32)) {
		// TODO: use more reliable approach
		// empty
		return nil
	}

	c, err := tlb.ToCell(s.State)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, c.Hash(2), s.Signature.Value) {
		log.Warn().Str("sig", base64.StdEncoding.EncodeToString(s.Signature.Value)).Msg("invalid signature")
		return fmt.Errorf("invalid signature")
	}
	return nil
}

var ErrNotFound = fmt.Errorf("not found")

func FindVirtualChannel(conditionals *cell.Dictionary, key ed25519.PublicKey) (*big.Int, *VirtualChannel, error) {
	return FindVirtualChannelWithProof(conditionals, key, nil)
}

func FindVirtualChannelWithProof(conditionals *cell.Dictionary, key ed25519.PublicKey, proofRoot *cell.ProofSkeleton) (*big.Int, *VirtualChannel, error) {
	// TODO: indexed dict o(1)

	var tempProofRoot *cell.ProofSkeleton
	if proofRoot != nil {
		tempProofRoot = cell.CreateProofSkeleton()
	}

	idx := big.NewInt(int64(binary.LittleEndian.Uint32(key)))
	sl, proofBranch, err := conditionals.LoadValueWithProof(cell.BeginCell().MustStoreBigUInt(idx, 32).EndCell(), tempProofRoot)
	if err != nil {
		if errors.Is(err, cell.ErrNoSuchKeyInDict) {
			if proofRoot != nil {
				proofRoot.Merge(tempProofRoot)
			}
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}

	vch, err := ParseVirtualChannelCond(sl)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse state of one of virtual channels")
	}

	if !bytes.Equal(vch.Key, key) {
		return nil, nil, ErrNotFound
	}

	if proofRoot != nil {
		proofBranch.SetRecursive()
		proofRoot.Merge(tempProofRoot)
	}
	return idx, vch, nil
}

func (s *SemiChannel) CheckSynchronized(with *SemiChannel) error {
	if !bytes.Equal(s.ChannelID, with.ChannelID) {
		return fmt.Errorf("diff channel id")
	}

	if with.CounterpartyData == nil {
		return fmt.Errorf("our state on their side is empty")
	}

	ourStateOnTheirSide, err := tlb.ToCell(with.CounterpartyData)
	if err != nil {
		return fmt.Errorf("failed to serialize our state on their side: %w", err)
	}
	ourState, err := tlb.ToCell(s.Data)
	if err != nil {
		return fmt.Errorf("failed to serialize our state: %w", err)
	}

	if !bytes.Equal(ourStateOnTheirSide.Hash(2), ourState.Hash()) {
		return fmt.Errorf("our state on their side is diff")
	}

	if s.CounterpartyData == nil {
		return fmt.Errorf("their state on our side is empty")
	}

	theirStateOnOurSide, err := tlb.ToCell(s.CounterpartyData)
	if err != nil {
		return fmt.Errorf("failed to serialize their state on our side: %w", err)
	}
	theirState, err := tlb.ToCell(with.Data)
	if err != nil {
		return fmt.Errorf("failed to serialize their state: %w", err)
	}

	if !bytes.Equal(theirStateOnOurSide.Hash(2), theirState.Hash(2)) {
		return fmt.Errorf("their state on our side is diff")
	}

	return nil
}

func (s *SemiChannel) Dump() string {
	c, err := tlb.ToCell(s.Data)
	if err != nil {
		return "failed cell"
	}

	cpData := "none"
	if s.CounterpartyData != nil {
		cp, err := tlb.ToCell(s.CounterpartyData)
		if err != nil {
			return "failed cell"
		}
		cpData = fmt.Sprintf("(data_hash: %s seqno: %d; sent: %s; conditionals_hash: %s)",
			base64.StdEncoding.EncodeToString(cp.Hash()[:8]),
			s.CounterpartyData.Seqno, s.CounterpartyData.Sent.String(), base64.StdEncoding.EncodeToString(s.CounterpartyData.ConditionalsHash))
	}

	return fmt.Sprintf("data_hash: %s seqno: %d; sent: %s; conditionals_hash: %s; counterparty: %s",
		base64.StdEncoding.EncodeToString(c.Hash()[:8]),
		s.Data.Seqno, s.Data.Sent.String(), base64.StdEncoding.EncodeToString(s.Data.ConditionalsHash), cpData)
}

func (s *SemiChannelBody) Copy() (SemiChannelBody, error) {
	return SemiChannelBody{
		Seqno:            s.Seqno,
		Sent:             s.Sent,
		ConditionalsHash: append([]byte{}, s.ConditionalsHash...),
	}, nil
}
